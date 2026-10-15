package embedding

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newMockServer 返回假 embedding 上游，handler 自定义响应。
func newMockServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(Options{BaseURL: srv.URL, APIKey: "sk-test", Model: "test-embed", Dimensions: 3})
	return c, srv
}

func okHandler(dims int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 校验请求格式（OpenAI 兼容协议）
		if r.URL.Path != "/embeddings" || r.Method != http.MethodPost {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		vec := make([]float32, dims)
		for i := range vec {
			vec[i] = 0.1 * float32(i+1)
		}
		fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[%s]}],"usage":{"total_tokens":3}}`,
			strings.Trim(strings.Join(strings.Fields(fmt.Sprint(vec)), ","), "[]"))
	}
}

func TestEmbedOK(t *testing.T) {
	c, _ := newMockServer(t, okHandler(3))
	vec, err := c.Embed(context.Background(), "你好")
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("期望 3 维，实际 %d", len(vec))
	}
}

func TestEmbedUpstreamError(t *testing.T) {
	c, _ := newMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	})
	_, err := c.Embed(context.Background(), "你好")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("应返回含状态码的错误: %v", err)
	}
}

func TestEmbedMalformedResponse(t *testing.T) {
	c, _ := newMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	})
	if _, err := c.Embed(context.Background(), "你好"); err == nil {
		t.Fatal("空 data 应返回错误")
	}
}

func TestEmbedDimensionMismatch(t *testing.T) {
	c, _ := newMockServer(t, okHandler(5)) // mock 返回 5 维，client 配置 3 维
	_, err := c.Embed(context.Background(), "你好")
	if err == nil || !strings.Contains(err.Error(), "维度不符") {
		t.Fatalf("维度不符应报错: %v", err)
	}
}

func TestEmbedContextCancel(t *testing.T) {
	c, _ := newMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // 慢上游
		okHandler(3)(w, r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Embed(ctx, "你好"); err == nil {
		t.Fatal("ctx 超时应返回错误")
	}
}

func TestCosine(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if got := Cosine(a, b); got < 0.9999 {
		t.Errorf("相同向量余弦应为 1，实际 %f", got)
	}
	c := []float32{0, 1, 0}
	if got := Cosine(a, c); got > 0.0001 {
		t.Errorf("正交向量余弦应为 0，实际 %f", got)
	}
	if got := Cosine(a, []float32{1, 2}); got != 0 {
		t.Errorf("维度不一致应返回 0，实际 %f", got)
	}
	if got := Cosine(a, make([]float32, 3)); got != 0 {
		t.Errorf("零向量应返回 0，实际 %f", got)
	}
}

// TestEmbedIntegration 真实调用硅基流动 bge-m3（需 EMBEDDING_API_KEY 环境变量）。
// 同时实测语义相似度阈值：同义句应 >0.8，无关句应 <0.5（为语义缓存 0.95 阈值选型提供数据）。
func TestEmbedIntegration(t *testing.T) {
	key := os.Getenv("EMBEDDING_API_KEY")
	if key == "" {
		t.Skip("无 EMBEDDING_API_KEY，跳过真实调用")
	}
	c := New(Options{
		BaseURL:    "https://api.siliconflow.cn/v1",
		APIKey:     key,
		Model:      "BAAI/bge-m3",
		Dimensions: 1024,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	embed := func(text string) []float32 {
		t.Helper()
		vec, err := c.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed(%q) 失败: %v", text, err)
		}
		return vec
	}

	q1 := embed("今天天气怎么样")
	q2 := embed("今天天气如何")   // 同义改写
	q3 := embed("红烧肉怎么做") // 无关

	simSame := Cosine(q1, q2)
	simDiff := Cosine(q1, q3)
	t.Logf("同义句相似度: %.4f（期望 >0.8）", simSame)
	t.Logf("无关句相似度: %.4f（期望 <0.5）", simDiff)
	if simSame < 0.8 {
		t.Errorf("同义句相似度过低: %.4f", simSame)
	}
	if simDiff > 0.5 {
		t.Errorf("无关句相似度过高: %.4f", simDiff)
	}
}
