package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/GenJi77JYXC/llm-gateway/internal/gateway"
)

// NewHTTPServer 组装 gin 引擎与带安全超时的 http.Server。
//
// 超时说明（LLM 场景与普通 API 不同）：
//   - ReadHeaderTimeout 5s：防慢速头攻击；
//   - ReadTimeout 30s：请求体通常很小，30s 足够；
//   - WriteTimeout 300s：流式生成长，给到 5 分钟上限（更长的流需调大或改用独立 server）；
//   - IdleTimeout 60s。
func NewHTTPServer(addr string, clientKeys []string, r *gateway.Router) *http.Server {
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())

	NewHandler(r).RegisterRoutes(engine, clientKeys)

	return &http.Server{
		Addr:              addr,
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
