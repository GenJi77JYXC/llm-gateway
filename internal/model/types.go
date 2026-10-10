// Package model 定义网关各层共享的领域类型（与任何框架/厂商解耦）。
package model

import "encoding/json"

// Usage 是 OpenAI 兼容的 token 用量。
// 非流式在响应体 usage 字段返回；
// 流式在最后一个 chunk（需请求 stream_options.include_usage=true）返回。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// APIError 是 OpenAI 风格的错误结构，所有上游错误最终都归一化成它。
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// ErrorBody 是 OpenAI 错误响应外层结构：{"error": {...}}。
type ErrorBody struct {
	Error APIError `json:"error"`
}

// ModelInfo 是 GET /v1/models 列表中的单项。
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// ModelList 是 GET /v1/models 的响应。
type ModelList struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// streamEnvelope 是流式 chunk 的最小解析结构，
// 只提取 usage 做计量，其余字段原样透传（见 openai provider）。
type streamEnvelope struct {
	Usage *Usage `json:"usage"`
}

// completionEnvelope 是非流式响应的最小解析结构。
type completionEnvelope struct {
	Usage *Usage `json:"usage"`
}

// ParseStreamUsage 从一个 SSE data 负载（不含 "data: " 前缀）中解析 usage。
// 非 usage chunk 或 [DONE] 返回 nil。
func ParseStreamUsage(payload []byte) *Usage {
	var env streamEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil
	}
	return env.Usage
}

// ParseCompletionUsage 从非流式响应体中解析 usage。
func ParseCompletionUsage(body []byte) *Usage {
	var env completionEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	return env.Usage
}
