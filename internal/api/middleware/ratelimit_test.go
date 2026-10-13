package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/GenJi77JYXC/llm-gateway/internal/config"
)

func setupRouter(l *RateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler := func(c *gin.Context) {
		// 验证 body 已被中间件恢复，handler 能完整读到
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil || len(raw) == 0 {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	if l != nil && l.Enabled() {
		r.POST("/v1/chat/completions", l.Middleware(), handler)
	} else {
		r.POST("/v1/chat/completions", handler)
	}
	return r
}

func do(r *gin.Engine, key, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// key 维度：burst 用完后第 3 个请求被 429
func TestKeyDimensionLimited(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{KeyQPS: 1, KeyBurst: 2})
	r := setupRouter(l)

	for i, want := range []int{200, 200, 429} {
		if got := do(r, "k1", chatBody); got != want {
			t.Fatalf("request %d: status = %d, want %d", i+1, got, want)
		}
	}
}

// key 维度：不同 key 的桶互相独立
func TestKeyDimensionIndependent(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{KeyQPS: 1, KeyBurst: 1})
	r := setupRouter(l)

	if got := do(r, "k1", chatBody); got != 200 {
		t.Fatalf("k1 first: status = %d, want 200", got)
	}
	if got := do(r, "k1", chatBody); got != 429 {
		t.Fatalf("k1 second: status = %d, want 429", got)
	}
	if got := do(r, "k2", chatBody); got != 200 {
		t.Fatalf("k2 first: status = %d, want 200（桶独立于 k1）", got)
	}
}

// model 维度：桶全局共享，不同 key 打同一 model 也受限
func TestModelDimensionSharedAcrossKeys(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{ModelQPS: 1, ModelBurst: 1})
	r := setupRouter(l)

	if got := do(r, "k1", chatBody); got != 200 {
		t.Fatalf("k1: status = %d, want 200", got)
	}
	if got := do(r, "k2", chatBody); got != 429 {
		t.Fatalf("k2 same model: status = %d, want 429（model 桶全局共享）", got)
	}
}

// model 维度：不同 model 的桶互相独立
func TestModelDimensionIndependent(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{ModelQPS: 1, ModelBurst: 1})
	r := setupRouter(l)

	if got := do(r, "k1", chatBody); got != 200 {
		t.Fatalf("m first: status = %d, want 200", got)
	}
	if got := do(r, "k1", chatBody); got != 429 {
		t.Fatalf("m second: status = %d, want 429", got)
	}
	otherBody := `{"model":"other","messages":[]}`
	if got := do(r, "k1", otherBody); got != 200 {
		t.Fatalf("other model: status = %d, want 200（桶独立）", got)
	}
}

// 两个维度都开启：先耗尽 key 桶返回 key 维度 429
func TestBothDimensions(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{
		KeyQPS: 1, KeyBurst: 1,
		ModelQPS: 100, ModelBurst: 100,
	})
	r := setupRouter(l)

	if got := do(r, "k1", chatBody); got != 200 {
		t.Fatalf("first: status = %d, want 200", got)
	}
	if got := do(r, "k1", chatBody); got != 429 {
		t.Fatalf("second: status = %d, want 429（key 维度先触发）", got)
	}
}

// 全零配置：Enabled 为 false，注册无中间件路由，全放行
func TestDisabledUnlimited(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{})
	if l.Enabled() {
		t.Fatal("Enabled() = true, want false（全零配置）")
	}
	r := setupRouter(l)
	for i := 0; i < 10; i++ {
		if got := do(r, "k1", chatBody); got != 200 {
			t.Fatalf("request %d: status = %d, want 200（未限流）", i+1, got)
		}
	}
}

// body 非 JSON：跳过 model 维度仍放行，且 body 完整交给 handler
func TestMalformedBodyFallsThrough(t *testing.T) {
	l := NewRateLimiter(config.RateLimitConfig{ModelQPS: 1, ModelBurst: 1})
	r := setupRouter(l)

	if got := do(r, "k1", "not-json"); got != 200 {
		t.Fatalf("status = %d, want 200（坏 body 跳过 model 维度，由 handler 处理）", got)
	}
}
