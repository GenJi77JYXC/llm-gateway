// Package embedding 封装 OpenAI 兼容的 embedding API 调用（零框架依赖）。
// 供应商无关：base_url + api_key + model 三要素配置驱动，
// 硅基流动（BAAI/bge-m3）、OpenAI（text-embedding-3-small）等均可接入。
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// defaultTimeout 单次 embedding 调用的总超时（含连接与读响应）。
const defaultTimeout = 10 * time.Second

// Options 是 Client 的构造参数。
type Options struct {
	BaseURL string // 如 https://api.siliconflow.cn/v1
	APIKey  string
	Model   string // 如 BAAI/bge-m3
	// Dimensions 向量维度，须与模型实际输出一致（Redis 向量索引创建依赖该值）
	Dimensions int
	// Timeout 单次调用超时，缺省 10s
	Timeout time.Duration
}

// Client 是 embedding API 客户端。
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	http       *http.Client
}

// New 创建 embedding 客户端（纯配置装配，不发请求，连通性在首次 Embed 时验证）。
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:    opts.BaseURL,
		apiKey:     opts.APIKey,
		model:      opts.Model,
		dimensions: opts.Dimensions,
		http:       &http.Client{Timeout: timeout},
	}
}

// Model 返回配置的模型名。
func (c *Client) Model() string { return c.model }

// Dimensions 返回配置的向量维度。
func (c *Client) Dimensions() int { return c.dimensions }

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed 将单条文本转为向量（维度 = Dimensions，不一致返回错误）。
// 请求绑定 ctx：调用方取消（如客户端断开）时立即终止上游请求。
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	payload, err := json.Marshal(embedRequest{Model: c.model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding 调用失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 上限 1MB，异常响应防爆内存
	if err != nil {
		return nil, fmt.Errorf("读取 embedding 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding 上游返回 %d: %s", resp.StatusCode, truncate(body, 200))
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 embedding 响应失败: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding 响应缺少向量数据")
	}

	vec := parsed.Data[0].Embedding
	if c.dimensions > 0 && len(vec) != c.dimensions {
		return nil, fmt.Errorf("embedding 维度不符: 配置 %d，实际 %d（请核对 model 与 dimensions）",
			c.dimensions, len(vec))
	}
	return vec, nil
}

// Cosine 计算两个向量的余弦相似度（-1 ~ 1）；维度不一致或零向量返回 0。
// 语义缓存命中判定（阈值 0.95）使用该值。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// truncate 截断错误响应体用于日志，避免超长 body 刷屏。
func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
