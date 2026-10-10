package gateway

import (
	"encoding/json"
	"fmt"
)

// PreparedRequest 是归一化后的请求。
type PreparedRequest struct {
	Model  string
	Stream bool
	Body   []byte
}

// PrepareChatBody 解析客户端原始请求体并做最小归一化：
//  1. 取出 model / stream 供路由与指标使用；
//  2. stream=true 时强制注入 stream_options.include_usage=true，
//     让上游在最后一个 chunk 返回准确 token 用量（见 one-api 阅读 Q9）。
//
// 其余字段（messages、tools、temperature 等）全部原样保留，不做语义改写。
func PrepareChatBody(raw []byte) (*PreparedRequest, error) {
	// 先解码到 map，保证未知字段不丢失
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	model, _ := body["model"].(string)
	if model == "" {
		return nil, fmt.Errorf("field 'model' is required")
	}

	stream, _ := body["stream"].(bool)
	if stream {
		opts, _ := body["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}

	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("re-marshal body: %w", err)
	}
	return &PreparedRequest{Model: model, Stream: stream, Body: out}, nil
}
