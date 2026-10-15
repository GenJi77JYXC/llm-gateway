package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newTestExact(t *testing.T) (*Exact, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	e, err := NewExact(ExactOptions{Addr: mr.Addr(), TTL: time.Hour})
	if err != nil {
		t.Fatalf("创建 Exact 失败: %v", err)
	}
	return e, mr
}

func TestKeyStable(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}`)
	k1, err := Key(body)
	if err != nil {
		t.Fatalf("Key 失败: %v", err)
	}
	k2, _ := Key(body)
	if k1 != k2 {
		t.Errorf("相同输入应得相同 key: %s != %s", k1, k2)
	}
}

func TestKeyIgnoresNonSemanticFields(t *testing.T) {
	k1, _ := Key([]byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}`))
	k2, _ := Key([]byte(`{"model":"deepseek-chat","temperature":0.7,"stream":false,"max_tokens":100,"messages":[{"role":"user","content":"你好"}]}`))
	if k1 != k2 {
		t.Errorf("temperature/stream/max_tokens 不应影响 key:\n%s\n%s", k1, k2)
	}
}

func TestKeyWhitespaceInsensitive(t *testing.T) {
	k1, _ := Key([]byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}`))
	k2, _ := Key([]byte("{\n  \"model\": \"deepseek-chat\",\n  \"messages\": [ { \"role\": \"user\", \"content\": \"你好\" } ]\n}"))
	if k1 != k2 {
		t.Errorf("JSON 空白差异不应影响 key:\n%s\n%s", k1, k2)
	}
}

func TestKeyDiffers(t *testing.T) {
	k1, _ := Key([]byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}`))
	k2, _ := Key([]byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"再见"}]}`))
	k3, _ := Key([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`))
	if k1 == k2 {
		t.Error("不同 messages 应得不同 key")
	}
	if k1 == k3 {
		t.Error("不同 model 应得不同 key")
	}
}

func TestKeyUncacheable(t *testing.T) {
	if _, err := Key([]byte(`{"model":"deepseek-chat"}`)); err == nil {
		t.Error("缺少 messages 应返回错误")
	}
	if _, err := Key([]byte(`not json`)); err == nil {
		t.Error("非法 JSON 应返回错误")
	}
}

func TestExactGetSet(t *testing.T) {
	e, mr := newTestExact(t)
	defer e.Close()
	ctx := context.Background()

	key := "llm:exact:testkey"
	// 未命中返回 (nil, nil)
	got, err := e.Get(ctx, key)
	if err != nil || got != nil {
		t.Fatalf("未命中应返回 (nil, nil): got=%v err=%v", got, err)
	}

	body := []byte(`{"id":"chatcmpl-1","choices":[]}`)
	if err := e.Set(ctx, key, body); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	got, err = e.Get(ctx, key)
	if err != nil || string(got) != string(body) {
		t.Fatalf("命中应返回原 body: got=%s err=%v", got, err)
	}

	// TTL 应接近 1h
	ttl := mr.TTL(key)
	if ttl <= 59*time.Minute || ttl > time.Hour {
		t.Errorf("TTL 异常: %v", ttl)
	}
}

func TestNewExactConnectFail(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	if _, err := NewExact(ExactOptions{Addr: addr, TTL: time.Hour}); err == nil {
		t.Fatal("Redis 不可达时应返回错误")
	}
}
