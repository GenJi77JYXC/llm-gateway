// Package middleware 存放 gin HTTP 中间件（仅 API 层允许依赖 gin）。
package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/GenJi77JYXC/llm-gateway/internal/config"
	"github.com/GenJi77JYXC/llm-gateway/internal/metrics"
	"github.com/GenJi77JYXC/llm-gateway/internal/model"
)

// RateLimiter 双维度令牌桶限流：按 client key + 按 model。
// 任一维度 QPS <= 0 表示该维度不限流；两个维度都关闭时 Enabled() 返回 false。
//
// bucket 数量上界：key 来自 auth 配置（未过鉴权的请求到不了这里），
// model 来自 providers 配置（未注册的 model 会被路由拒绝），均有界，无需淘汰。
type RateLimiter struct {
	keyQPS     float64
	keyBurst   int
	modelQPS   float64
	modelBurst int

	mu           sync.Mutex
	keyBuckets   map[string]*rate.Limiter
	modelBuckets map[string]*rate.Limiter
}

// NewRateLimiter 按配置构造限流器。cfg 应已过 config 归一化（burst 缺省补齐）。
func NewRateLimiter(cfg config.RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		keyQPS:       cfg.KeyQPS,
		keyBurst:     cfg.KeyBurst,
		modelQPS:     cfg.ModelQPS,
		modelBurst:   cfg.ModelBurst,
		keyBuckets:   make(map[string]*rate.Limiter),
		modelBuckets: make(map[string]*rate.Limiter),
	}
}

// Enabled 报告是否至少启用一个维度。
func (l *RateLimiter) Enabled() bool {
	return l.keyQPS > 0 || l.modelQPS > 0
}

// Middleware 限流中间件，仅挂在 POST /v1/chat/completions。
// 超限返回 429（OpenAI 风格错误），放行则正常进入 handler。
func (l *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if l.keyQPS > 0 && !l.bucket(l.keyBuckets, key, l.keyQPS, l.keyBurst).Allow() {
			reject(c, "key")
			return
		}

		if l.modelQPS > 0 {
			// 轻量解析 body 取 model 字段，随后恢复 body 交给 handler 完整处理；
			// body 不可读/不可解析时跳过 model 维度（handler 会返回 400）
			if raw, err := io.ReadAll(c.Request.Body); err == nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(raw))
				var probe struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(raw, &probe) == nil && probe.Model != "" {
					if !l.bucket(l.modelBuckets, probe.Model, l.modelQPS, l.modelBurst).Allow() {
						reject(c, "model")
						return
					}
				}
			}
		}
		c.Next()
	}
}

// bucket 取对应名字的令牌桶，不存在则按限流参数新建（惰性初始化）。
func (l *RateLimiter) bucket(m map[string]*rate.Limiter, name string, qps float64, burst int) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := m[name]
	if !ok {
		b = rate.NewLimiter(rate.Limit(qps), burst)
		m[name] = b
	}
	return b
}

func reject(c *gin.Context, dimension string) {
	metrics.RateLimitedTotal.WithLabelValues(dimension).Inc()
	c.AbortWithStatusJSON(http.StatusTooManyRequests, model.ErrorBody{
		Error: model.APIError{
			Message: "rate limit exceeded, please slow down",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		},
	})
}
