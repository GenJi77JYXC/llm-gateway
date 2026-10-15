package usage

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*RedisRecorder, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	r, err := NewRedisRecorder(RedisOptions{Addr: mr.Addr()})
	if err != nil {
		t.Fatalf("创建 RedisRecorder 失败: %v", err)
	}
	return r, mr
}

// hGetAll 用独立的 client 读取计量 hash（Close 会关闭 recorder 自己的 client）。
func hGetAll(t *testing.T, addr, key string) map[string]string {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	res, err := client.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("读取聚合结果失败: %v", err)
	}
	return res
}

func TestRedisRecorderFlushAggregate(t *testing.T) {
	r, mr := newTestRedis(t)
	defer r.Close()

	r.flush([]Event{
		{KeyID: "dev", Model: "deepseek-chat", PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
		{KeyID: "dev", Model: "deepseek-chat", PromptTokens: 11, CompletionTokens: 5, TotalTokens: 16},
		{KeyID: "ci", Model: "gpt-4o", PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	})

	key := usageKeyPrefix + time.Now().Format(dayLayout)
	got := hGetAll(t, mr.Addr(), key)
	want := map[string]string{
		"dev|deepseek-chat|prompt":     "19",
		"dev|deepseek-chat|completion": "6",
		"dev|deepseek-chat|total":      "25",
		"ci|gpt-4o|prompt":             "3",
		"ci|gpt-4o|completion":         "4",
		"ci|gpt-4o|total":              "7",
	}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 个 field，实际 %d: %v", len(want), len(got), got)
	}
	for f, v := range want {
		if got[f] != v {
			t.Errorf("field %s 期望 %s，实际 %s", f, v, got[f])
		}
	}

	ttl := mr.TTL(key)
	if ttl <= 0 {
		t.Errorf("hash 应设置过期时间，TTL=%v", ttl)
	}
}

func TestRedisRecorderCloseDrains(t *testing.T) {
	r, mr := newTestRedis(t)

	// 连续投递超过单批容量，验证异步链路与 Close 排干不丢事件
	const n = 200
	for i := 0; i < n; i++ {
		r.Record(Event{KeyID: "dev", Model: "m", PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	key := usageKeyPrefix + time.Now().Format(dayLayout)
	got := hGetAll(t, mr.Addr(), key)
	if got["dev|m|prompt"] != "200" || got["dev|m|completion"] != "200" || got["dev|m|total"] != "400" {
		t.Errorf("Close 后应全部落盘: %v", got)
	}
}

func TestRedisRecorderConnectFail(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // 先拿到端口再关掉，模拟 Redis 不可达

	if _, err := NewRedisRecorder(RedisOptions{Addr: addr}); err == nil {
		t.Fatal("Redis 不可达时应返回错误")
	}
}

func TestRedisRecorderDailyUsage(t *testing.T) {
	r, _ := newTestRedis(t)
	defer r.Close()

	r.flush([]Event{
		{KeyID: "dev", Model: "deepseek-chat", PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
		{KeyID: "ci", Model: "gpt-4o", PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	})

	got, err := r.DailyUsage(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("DailyUsage 失败: %v", err)
	}
	if got["dev|deepseek-chat|total"] != 9 || got["ci|gpt-4o|prompt"] != 3 {
		t.Errorf("DailyUsage 读取错误: %v", got)
	}

	// 无数据的日期返回空 map 而非错误
	empty, err := r.DailyUsage(context.Background(), time.Now().AddDate(0, 0, -3))
	if err != nil || len(empty) != 0 {
		t.Errorf("无数据日期应返回空 map: len=%d err=%v", len(empty), err)
	}
}
