// Command benchmark 是网关压测工具：N 个 worker 闭环打 chat/completions，
// 客户端侧统计吞吐、延迟分位与流式 TTFT，可与 /metrics 服务端数据对账。
//
// 用法：
//
//	go run ./scripts/benchmark -target http://localhost:18080 -key sk-xxx -model mock-model -workers 20 -duration 15s
//	go run ./scripts/benchmark -target http://localhost:18080 -key sk-xxx -model mock-model -stream -workers 20 -duration 15s
//
// 直连上游测基线（无鉴权）：
//
//	go run ./scripts/benchmark -target http://localhost:18081 -model mock-model -workers 20 -duration 15s
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latency time.Duration
	ttft    time.Duration // 仅流式：首个 data chunk 到达时间
	err     bool
}

func main() {
	target := flag.String("target", "http://localhost:18080", "base URL of gateway or upstream")
	key := flag.String("key", "", "client API key (Authorization: Bearer), empty for direct upstream")
	model := flag.String("model", "mock-model", "model name")
	stream := flag.Bool("stream", false, "use streaming requests")
	workers := flag.Int("workers", 10, "concurrent workers (closed loop)")
	duration := flag.Duration("duration", 10*time.Second, "benchmark duration")
	flag.Parse()

	body, _ := json.Marshal(map[string]any{
		"model":    *model,
		"stream":   *stream,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})

	client := &http.Client{
		Timeout: 0, // 流式响应不能用整体超时，靠 ctx 控制
		Transport: &http.Transport{
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 1024,
		},
	}

	var (
		resultsMu sync.Mutex
		results   []result
		inflight  int64
		maxInfl   int64
	)
	deadline := time.Now().Add(*duration)
	var wg sync.WaitGroup

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				n := atomic.AddInt64(&inflight, 1)
				for {
					old := atomic.LoadInt64(&maxInfl)
					if n <= old || atomic.CompareAndSwapInt64(&maxInfl, old, n) {
						break
					}
				}
				r := doOnce(client, *target, *key, body, *stream)
				atomic.AddInt64(&inflight, -1)
				resultsMu.Lock()
				results = append(results, r)
				resultsMu.Unlock()
			}
		}()
	}
	wg.Wait()

	report(results, *duration, *workers, *stream, maxInfl)
}

// doOnce 发一次请求并测量延迟（流式额外测 TTFT）。
func doOnce(client *http.Client, target, key string, body []byte, stream bool) result {
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, target+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return result{err: true}
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := client.Do(req)
	if err != nil {
		return result{latency: time.Since(start), err: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return result{latency: time.Since(start), err: true}
	}

	if !stream {
		_, _ = io.Copy(io.Discard, resp.Body)
		return result{latency: time.Since(start)}
	}

	// 流式：逐行扫，记录首个 data chunk 时间，直到 [DONE] 或出错
	var ttft time.Duration
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) >= 5 && string(line[:5]) == "data:" {
			if ttft == 0 {
				ttft = time.Since(start)
			}
			if bytes.Contains(line, []byte("[DONE]")) {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result{latency: time.Since(start), ttft: ttft, err: true}
	}
	return result{latency: time.Since(start), ttft: ttft}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1)*p + 0.5)
	return sorted[idx]
}

func report(results []result, dur time.Duration, workers int, stream bool, maxInfl int64) {
	var oks, errs int
	lats := make([]time.Duration, 0, len(results))
	ttfts := make([]time.Duration, 0, len(results))
	for _, r := range results {
		if r.err {
			errs++
			continue
		}
		oks++
		lats = append(lats, r.latency)
		if stream && r.ttft > 0 {
			ttfts = append(ttfts, r.ttft)
		}
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })

	fmt.Println("---- benchmark result ----")
	fmt.Printf("duration=%s workers=%d stream=%v max_inflight=%d\n", dur, workers, stream, maxInfl)
	fmt.Printf("requests=%d ok=%d error=%d (%.2f%%)\n", len(results), oks, errs, 100*float64(errs)/float64(max(len(results), 1)))
	fmt.Printf("throughput=%.1f req/s\n", float64(oks)/dur.Seconds())
	if oks > 0 {
		fmt.Printf("latency p50=%s p90=%s p99=%s max=%s\n",
			percentile(lats, 0.5), percentile(lats, 0.9), percentile(lats, 0.99), lats[len(lats)-1])
	}
	if stream && len(ttfts) > 0 {
		fmt.Printf("ttft    p50=%s p90=%s p99=%s max=%s\n",
			percentile(ttfts, 0.5), percentile(ttfts, 0.9), percentile(ttfts, 0.99), ttfts[len(ttfts)-1])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
