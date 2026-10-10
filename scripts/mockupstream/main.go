// Command mockupstream 是本地联调用的假 OpenAI 兼容上游，不需要真实 API Key。
// 支持非流式与 SSE 流式响应（含 usage chunk），Week 2 还会用它模拟 429/500/断流。
//
// 用法：go run ./scripts/mockupstream -addr :18081
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func main() {
	addr := flag.String("addr", ":18081", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "chatcmpl-mock",
				"object": "chat.completion",
				"model":  req.Model,
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "你好，我是 mock 上游的非流式回复。",
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":     11,
					"completion_tokens": 7,
					"total_tokens":      18,
				},
			})
			return
		}

		// SSE 流式：3 个内容 chunk + 1 个 usage chunk + [DONE]
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeChunk := func(content string, usage map[string]any) {
			chunk := map[string]any{
				"id":     "chatcmpl-mock",
				"object": "chat.completion.chunk",
				"model":  req.Model,
			}
			if content != "" {
				chunk["choices"] = []map[string]any{{
					"index":         0,
					"delta":         map[string]any{"content": content},
					"finish_reason": nil,
				}}
			} else {
				chunk["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}}}
			}
			if usage != nil {
				chunk["usage"] = usage
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}

		for _, token := range []string{"你", "好", "！"} {
			time.Sleep(50 * time.Millisecond) // 让 TTFT 可观测
			writeChunk(token, nil)
		}
		writeChunk("", map[string]any{
			"prompt_tokens":     9,
			"completion_tokens": 3,
			"total_tokens":      12,
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	log.Printf("mock upstream listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
