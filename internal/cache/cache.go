// Package cache 提供 LLM 响应缓存，零框架依赖。
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 精确缓存的键前缀与 Redis 操作超时。
const (
	exactKeyPrefix = "llm:exact:"
	dialTimeout    = 3 * time.Second
	opTimeout      = time.Second
)

// ExactOptions 是 Exact 的构造参数。
type ExactOptions struct {
	Addr     string
	Password string
	DB       int
	// TTL 缓存有效期（roadmap 默认 1h）
	TTL time.Duration
}

// Exact 是精确缓存：完整响应 body 按 prompt 哈希存取，TTL 到期自动清理。
// 只缓存非流式请求的成功响应（流式缓存重放复杂度高，roadmap 既定取舍）。
type Exact struct {
	client *redis.Client
	ttl    time.Duration
}

// NewExact 创建精确缓存并 Ping 探测；连不上返回错误，由调用方决定降级（无缓存直连上游）。
func NewExact(opts ExactOptions) (*Exact, error) {
	if opts.TTL <= 0 {
		return nil, fmt.Errorf("exact cache TTL 必须为正数")
	}
	client := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  dialTimeout,
		ReadTimeout:  opTimeout,
		WriteTimeout: opTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+opTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &Exact{client: client, ttl: opts.TTL}, nil
}

// Key 从请求体计算精确缓存键：SHA256(model + "\n" + compact(messages))。
//
// 只取 model 与 messages 两个字段，temperature / stream / max_tokens 等不参与：
// 缓存语义本来就是"同 prompt 返回确定结果"，这些字段不应把缓存打散（roadmap T3.4 既定策略）。
// messages 先做 JSON compact（去空白），消除客户端格式化差异。
// body 缺少 model/messages 时返回错误，调用方按不可缓存处理。
func Key(raw []byte) (string, error) {
	var body struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	if body.Model == "" || len(body.Messages) == 0 {
		return "", errors.New("请求体缺少 model 或 messages，不可缓存")
	}

	messages := body.Messages
	var buf bytes.Buffer
	if err := json.Compact(&buf, body.Messages); err == nil {
		messages = buf.Bytes()
	}

	sum := sha256.Sum256(append([]byte(body.Model+"\n"), messages...))
	return exactKeyPrefix + hex.EncodeToString(sum[:]), nil
}

// Get 查询缓存；未命中返回 (nil, nil)，Redis 故障返回错误（调用方按 miss 降级）。
func (e *Exact) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := e.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return val, err
}

// Set 写入缓存（带 TTL）。失败返回错误，由调用方决定是否仅记日志。
func (e *Exact) Set(ctx context.Context, key string, body []byte) error {
	return e.client.Set(ctx, key, body, e.ttl).Err()
}

// Close 关闭底层连接。
func (e *Exact) Close() error {
	return e.client.Close()
}
