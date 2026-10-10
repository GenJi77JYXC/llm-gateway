// Package openai 实现 OpenAI 兼容协议的 Provider。
// DeepSeek、OpenAI、通义千问等所有兼容 /v1/chat/completions 的上游
// 仅靠配置中的 base_url / api_key / models 区分，复用本实现。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GenJi77JYXC/llm-gateway/internal/model"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// Config 是 OpenAI 兼容上游的配置。
type Config struct {
	Name    string
	BaseURL string // 如 https://api.deepseek.com/v1
	APIKey  string
	Models  []string
}

// Provider 是 OpenAI 兼容协议实现。
type Provider struct {
	cfg    Config
	client *http.Client
}

// New 创建一个 OpenAI 兼容 Provider。
func New(cfg Config, client *http.Client) *Provider {
	if client == nil {
		client = DefaultHTTPClient()
	}
	return &Provider{cfg: cfg, client: client}
}

// DefaultHTTPClient 构造适合 LLM 长连接场景的 HTTP 客户端：
// 不设整体超时（由 ctx 的取消信号控制生命周期），但调大连接池与空闲连接复用。
func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

func (p *Provider) Name() string    { return p.cfg.Name }
func (p *Provider) Models() []string { return p.cfg.Models }

// newRequest 构造上游请求。
// 关键：使用 NewRequestWithContext 绑定调用方 ctx，
// 客户端断开时 ctx cancel，上游连接立即关闭（one-api 在这点上是缺失的）。
func (p *Provider) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	url := p.cfg.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	return req, nil
}

// Chat 非流式调用。
func (p *Provider) Chat(ctx context.Context, in *provider.ChatRequest) (*provider.Result, error) {
	req, err := p.newRequest(ctx, in.Body)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider %s request: %w", p.cfg.Name, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("provider %s read body: %w", p.cfg.Name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &provider.UpstreamError{StatusCode: resp.StatusCode, Body: data}
	}
	return &provider.Result{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       data,
		Usage:      model.ParseCompletionUsage(data),
	}, nil
}

// ChatStream 流式调用。状态码非 2xx 时读完全部 body 返回 UpstreamError；
// 2xx 时返回 Stream 读取器，由调用方边读边转发。
func (p *Provider) ChatStream(ctx context.Context, in *provider.ChatRequest) (provider.Stream, error) {
	req, err := p.newRequest(ctx, in.Body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider %s request: %w", p.cfg.Name, err)
	}

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &provider.UpstreamError{StatusCode: resp.StatusCode, Body: data}
	}

	scanner := bufio.NewScanner(resp.Body)
	// 推理模型单个 SSE chunk 可能很大，放大 scanner 缓冲到 16MB
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &streamReader{
		resp:    resp,
		scanner: scanner,
	}, nil
}

// streamReader 实现 provider.Stream。
type streamReader struct {
	resp    *http.Response
	scanner *bufio.Scanner
}

func (s *streamReader) StatusCode() int       { return s.resp.StatusCode }
func (s *streamReader) Header() http.Header   { return s.resp.Header }

func (s *streamReader) Close() error          { return s.resp.Body.Close() }

// Next 逐条读取 SSE data 事件。
func (s *streamReader) Next() ([]byte, *model.Usage, bool, error) {
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			// 忽略空行与 SSE 注释行（": keep-alive" 等）
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil, nil, true, nil
		}
		// 旁路解析 usage，payload 本身原样返回给上层透传（不重新序列化）
		return []byte(payload), model.ParseStreamUsage([]byte(payload)), false, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, nil, false, err
	}
	// 上游正常结束但未发 [DONE]
	return nil, nil, false, io.EOF
}
