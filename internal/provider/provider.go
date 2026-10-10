// Package provider 定义模型上游的抽象层。
// 该包只依赖标准库 context 和 internal/model，禁止 import gin 等 HTTP 框架，
// 新增上游厂商只需实现 Provider 接口，路由/负载均衡/熔断层均无需改动。
package provider

import (
	"context"
	"net/http"

	"github.com/GenJi77JYXC/llm-gateway/internal/model"
)

// ChatRequest 是传给 Provider 的统一请求。
// Body 是已经完成协议归一化、可直接发给上游的 JSON（含 stream_options 注入）。
type ChatRequest struct {
	// Model 客户端请求的原始模型名（用于日志/指标）
	Model string
	// Stream 是否为流式请求
	Stream bool
	// Body 最终发给上游的 JSON 请求体
	Body []byte
}

// Result 是非流式调用结果。Body 为上游原始响应体，由 API 层原样透传。
type Result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Usage      *model.Usage
}

// UpstreamError 表示上游返回了非 2xx 响应，API 层据此把状态码与响应体透传给客户端。
type UpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *UpstreamError) Error() string {
	return "upstream returned status " + http.StatusText(e.StatusCode)
}

// Stream 是流式响应的读取器。用法：
//
//	for {
//	    raw, usage, done, err := s.Next()
//	    ...
//	}
//	s.Close()
type Stream interface {
	// StatusCode 返回上游 HTTP 状态码（Next 之前即可读取）
	StatusCode() int
	// Header 返回上游响应头
	Header() http.Header
	// Next 读取下一条 SSE data 负载。
	// raw 为原始 data 内容（不含 "data: " 前缀，也不含 [DONE]）；
	// usage 在携带用量的 chunk 上非 nil；
	// done=true 表示上游发送了 [DONE]；err 非 nil 表示读取异常（连接中断等）。
	Next() (raw []byte, usage *model.Usage, done bool, err error)
	// Close 释放上游连接
	Close() error
}

// Provider 是模型上游的统一抽象。
type Provider interface {
	// Name 返回上游实例名（配置中的 name），用于日志与指标 label
	Name() string
	// Models 返回该上游支持的模型列表
	Models() []string
	// Chat 发起非流式请求
	Chat(ctx context.Context, req *ChatRequest) (*Result, error)
	// ChatStream 发起流式请求，返回 SSE 读取器
	ChatStream(ctx context.Context, req *ChatRequest) (Stream, error)
}
