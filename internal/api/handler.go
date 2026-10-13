// Package api 是 HTTP 适配层（唯一允许 import gin 的层）。
// 职责：HTTP 请求 <-> 领域类型翻译、SSE 透传、指标埋点。
// 核心业务（路由、协议、Provider）不在本层。
package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/GenJi77JYXC/llm-gateway/internal/api/middleware"
	"github.com/GenJi77JYXC/llm-gateway/internal/gateway"
	"github.com/GenJi77JYXC/llm-gateway/internal/metrics"
	"github.com/GenJi77JYXC/llm-gateway/internal/model"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// Handler 持有核心层依赖。
type Handler struct {
	router *gateway.Router
}

// NewHandler 创建 HTTP Handler。
func NewHandler(r *gateway.Router) *Handler {
	return &Handler{router: r}
}

// RegisterRoutes 在给定 gin 引擎上注册全部路由。
func (h *Handler) RegisterRoutes(engine *gin.Engine, clientKeys []string) {
	engine.GET("/healthz", h.healthz)
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))

	v1 := engine.Group("/v1")
	v1.Use(middleware.ClientAuth(clientKeys))
	{
		v1.GET("/models", h.listModels)
		v1.POST("/chat/completions", h.chatCompletions)
	}
}

func (h *Handler) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) listModels(c *gin.Context) {
	models := h.router.Models()
	data := make([]model.ModelInfo, 0, len(models))
	for _, m := range models {
		data = append(data, model.ModelInfo{
			ID:      m,
			Object:  "model",
			OwnedBy: h.router.Owner(m),
		})
	}
	c.JSON(http.StatusOK, model.ModelList{Object: "list", Data: data})
}

// chatCompletions 是网关主入口。
func (h *Handler) chatCompletions(c *gin.Context) {
	startedAt := time.Now()

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "")
		return
	}

	prepared, err := gateway.PrepareChatBody(raw)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}

	streamLabel := metrics.StreamLabel(prepared.Stream)
	metrics.InflightRequests.WithLabelValues(streamLabel).Inc()
	defer metrics.InflightRequests.WithLabelValues(streamLabel).Dec()

	req := &provider.ChatRequest{
		Model:  prepared.Model,
		Stream: prepared.Stream,
		Body:   prepared.Body,
	}

	if prepared.Stream {
		h.handleStream(c, req, prepared.Model, startedAt)
		return
	}
	h.handleBlocking(c, req, prepared.Model, startedAt)
}

// handleBlocking 处理非流式请求：上游 body 原样透传。fallback 在 gateway 层完成。
func (h *Handler) handleBlocking(
	c *gin.Context,
	req *provider.ChatRequest,
	modelName string,
	startedAt time.Time,
) {
	res, providerName, err := h.router.Chat(c.Request.Context(), modelName, req)
	if err != nil {
		var notFound *gateway.ErrModelNotFound
		if errors.As(err, &notFound) {
			// 未知模型不计指标：model 来自用户输入，防 Prometheus label 基数爆炸
			writeAPIError(c, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_not_found")
			return
		}
	}
	if providerName == "" {
		providerName = "unknown"
	}

	result := "ok"
	defer func() {
		metrics.RequestsTotal.WithLabelValues(providerName, modelName, "false", result).Inc()
		metrics.RequestDuration.WithLabelValues(providerName, modelName, "false").
			Observe(time.Since(startedAt).Seconds())
	}()

	if err != nil {
		result = h.writeCallError(c, err)
		return
	}

	recordUsage(providerName, modelName, res.Usage)
	if ct := res.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Data(res.StatusCode, res.Header.Get("Content-Type"), res.Body)
}

// handleStream 处理流式请求：边解析边透传原始 chunk，旁路采集 TTFT 与 usage。
// fallback 仅发生在流建立之前；流建立后中途失败按 interrupted 处理（one-api Q15）。
func (h *Handler) handleStream(
	c *gin.Context,
	req *provider.ChatRequest,
	modelName string,
	startedAt time.Time,
) {
	stream, providerName, err := h.router.ChatStream(c.Request.Context(), modelName, req)
	if err != nil {
		var notFound *gateway.ErrModelNotFound
		if errors.As(err, &notFound) {
			writeAPIError(c, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_not_found")
			return
		}
	}
	if providerName == "" {
		providerName = "unknown"
	}
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(providerName, modelName, "true", h.writeCallError(c, err)).Inc()
		metrics.RequestDuration.WithLabelValues(providerName, modelName, "true").
			Observe(time.Since(startedAt).Seconds())
		return
	}
	defer stream.Close()

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		// 理论上不会发生；ResponseWriter 必须支持 Flush 才能做 SSE
		writeAPIError(c, http.StatusInternalServerError, "streaming unsupported by server", "gateway_error", "")
		metrics.RequestsTotal.WithLabelValues(providerName, modelName, "true", "internal_error").Inc()
		return
	}

	// SSE 响应头一旦写出，就不能再回退为错误响应，也不能重试上游（见 one-api 阅读 Q15）
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // 关闭 Nginx 缓冲
	c.Writer.WriteHeader(stream.StatusCode())
	flusher.Flush()

	result := "ok"
	firstChunk := true
	doneRendered := false

	finish := func() {
		if !doneRendered {
			// 上游未正常发 [DONE]（包括正常 EOF），补发一个保证客户端能收尾
			_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		}
		metrics.RequestsTotal.WithLabelValues(providerName, modelName, "true", result).Inc()
		metrics.RequestDuration.WithLabelValues(providerName, modelName, "true").
			Observe(time.Since(startedAt).Seconds())
	}

	for {
		raw, usage, done, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				finish()
				return
			}
			// 上游流中途异常：明确给客户端一个错误事件，避免静默截断（one-api 阅读 Q10 的改进点）
			result = "interrupted"
			metrics.StreamInterruptTotal.WithLabelValues(providerName, modelName).Inc()
			writeStreamError(c.Writer, flusher, "upstream stream interrupted: "+err.Error())
			finish()
			return
		}
		if firstChunk {
			metrics.TTFT.WithLabelValues(providerName, modelName).
				Observe(time.Since(startedAt).Seconds())
			firstChunk = false
		}
		if usage != nil {
			recordUsage(providerName, modelName, usage)
		}
		if done {
			doneRendered = true
			_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			finish()
			return
		}
		// raw 是上游原始 data 负载，直接透传，不重新序列化
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
		flusher.Flush()
	}
}

// writeCallError 把上游/传输错误归一化为 OpenAI 风格响应，返回指标 result 标签。
func (h *Handler) writeCallError(c *gin.Context, err error) string {
	var allDown *gateway.ErrAllUpstreamDown
	if errors.As(err, &allDown) {
		writeAPIError(c, http.StatusServiceUnavailable, err.Error(), "upstream_error", "all_upstreams_unavailable")
		return "upstream_error"
	}
	var upstreamErr *provider.UpstreamError
	if errors.As(err, &upstreamErr) {
		// 上游有响应体就原样透传（多为 OpenAI 风格错误 JSON），否则包装一层
		if len(upstreamErr.Body) > 0 {
			c.Data(upstreamErr.StatusCode, "application/json; charset=utf-8", upstreamErr.Body)
		} else {
			c.JSON(upstreamErr.StatusCode, model.ErrorBody{Error: model.APIError{
				Message: fmt.Sprintf("upstream returned status %d", upstreamErr.StatusCode),
				Type:    "upstream_error",
			}})
		}
		return "upstream_error"
	}
	// 传输层错误（DNS/连接失败/ctx 取消等）
	writeAPIError(c, http.StatusBadGateway, err.Error(), "upstream_error", "bad_gateway")
	return "internal_error"
}

// writeAPIError 写出 OpenAI 风格错误。
func writeAPIError(c *gin.Context, status int, message, errType, code string) {
	c.JSON(status, model.ErrorBody{Error: model.APIError{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
}

// writeStreamError 在 SSE 流中写入一个错误事件。
func writeStreamError(w http.ResponseWriter, flusher http.Flusher, message string) {
	payload := fmt.Sprintf(
		`data: {"error":{"message":%q,"type":"upstream_error","code":"stream_interrupted"}}`+"\n\n",
		message,
	)
	_, _ = w.Write([]byte(payload))
	flusher.Flush()
}

func recordUsage(providerName, modelName string, usage *model.Usage) {
	if usage == nil {
		return
	}
	metrics.TokensTotal.WithLabelValues(providerName, modelName, "prompt").
		Add(float64(usage.PromptTokens))
	metrics.TokensTotal.WithLabelValues(providerName, modelName, "completion").
		Add(float64(usage.CompletionTokens))
	metrics.TokensTotal.WithLabelValues(providerName, modelName, "total").
		Add(float64(usage.TotalTokens))
}
