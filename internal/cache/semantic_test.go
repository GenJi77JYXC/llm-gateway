package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/GenJi77JYXC/llm-gateway/internal/embedding"
	"github.com/redis/go-redis/v9"
)

// fakeEmbedder 按文本返回预设向量（确定性），calls 记录调用次数。
type fakeEmbedder struct {
	vecs  map[string][]float32
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	v, ok := f.vecs[text]
	if !ok {
		return nil, fmt.Errorf("fakeEmbedder: 未预设的文本 %q", text)
	}
	return v, nil
}

// unitVec 生成第 i 位为 1 的 n 维单位向量。
func unitVec(n, i int) []float32 {
	v := make([]float32, n)
	v[i] = 1
	return v
}

// similarVec 生成与 e1 余弦相似度约 sim 的 n 维向量（e1 与 e2 的凸组合）。
func similarVec(n int, sim float64) []float32 {
	v := make([]float32, n)
	v[0] = float32(sim)
	v[1] = float32(1 - sim)
	return v
}

const testDims = 8 // 测试用小维度（真实 bge-m3 为 1024，逻辑一致）

// newTestSemantic 连接真实 redis-stack（默认 127.0.0.1:6380），不可达则 skip。
// miniredis 不支持 RediSearch 模块命令，语义缓存必须用真实 redis-stack 验证。
// 测试结束 DROP 索引，避免不同维度（testDims=8 vs 真实 1024）的测试互相污染。
func newTestSemantic(t *testing.T, embedder Embedder) *Semantic {
	t.Helper()
	addr := "127.0.0.1:6380"
	s, err := NewSemantic(SemanticOptions{
		Addr: addr, TTL: time.Hour, Threshold: 0.95, Dimensions: testDims, Embedder: embedder,
	})
	if err != nil {
		t.Skipf("redis-stack 不可达（%v），跳过语义缓存测试", err)
	}
	t.Cleanup(func() {
		s.client.Do(context.Background(), "FT.DROPINDEX", semanticIndex)
		_ = s.Close()
	})
	return s
}

func TestSemanticLookupHit(t *testing.T) {
	e := &fakeEmbedder{vecs: map[string][]float32{
		"如何学习 Go 语言": unitVec(testDims, 0),
		"Go 语言怎么入门":  similarVec(testDims, 0.99), // 与上句高度相似
		"今天吃什么":      unitVec(testDims, 2),       // 正交无关
	}}
	s := newTestSemantic(t, e)
	ctx := context.Background()

	body := []byte(`{"id":"r1","choices":[{"message":{"content":"官方文档 + 写项目"}}]}`)
	if err := s.Store(ctx, "test-model", "如何学习 Go 语言", e.vecs["如何学习 Go 语言"], body); err != nil {
		t.Fatalf("Store 失败: %v", err)
	}
	t.Cleanup(func() { s.client.Del(ctx, semanticKey("test-model", "如何学习 Go 语言")) })

	// 同义改写应命中（相似度 0.99 > 阈值 0.95）
	hit, vec, err := s.Lookup(ctx, "test-model", "Go 语言怎么入门")
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if string(hit) != string(body) {
		t.Errorf("同义句应命中缓存，实际 hit=%s", hit)
	}
	if vec == nil {
		t.Error("miss/hit 时 vec 均应非 nil（供 Store 复用）")
	}

	// 无关句应 miss
	hit, _, err = s.Lookup(ctx, "test-model", "今天吃什么")
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if hit != nil {
		t.Errorf("无关句不应命中，实际 %s", hit)
	}
}

func TestSemanticModelIsolation(t *testing.T) {
	e := &fakeEmbedder{vecs: map[string][]float32{
		"如何学习 Go 语言": unitVec(testDims, 0),
	}}
	s := newTestSemantic(t, e)
	ctx := context.Background()

	// 相同 prompt 存到 model-a
	if err := s.Store(ctx, "model-a", "如何学习 Go 语言", e.vecs["如何学习 Go 语言"], []byte(`{"r":"a"}`)); err != nil {
		t.Fatalf("Store 失败: %v", err)
	}
	t.Cleanup(func() { s.client.Del(ctx, semanticKey("model-a", "如何学习 Go 语言")) })

	// model-b 查询完全相同向量也不应命中（TAG 隔离）
	hit, _, err := s.Lookup(ctx, "model-b", "如何学习 Go 语言")
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if hit != nil {
		t.Errorf("不同 model 不应共享语义缓存，实际命中 %s", hit)
	}
}

func TestMessagesText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "单轮",
			body: `{"model":"m","messages":[{"role":"user","content":"你好"}]}`,
			want: "user: 你好\n",
		},
		{
			name: "多轮保序",
			body: `{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"}]}`,
			want: "system: 你是助手\nuser: 你好\nassistant: 你好！\n",
		},
		{
			name: "多模态 content 用原始 JSON",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"看图"}]}]}`,
			want: `user: [{"type":"text","text":"看图"}]` + "\n",
		},
		{name: "无 messages", body: `{"model":"m"}`, want: ""},
		{name: "非法 JSON", body: `not json`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MessagesText([]byte(tc.body)); got != tc.want {
				t.Errorf("期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

func TestEscapeTag(t *testing.T) {
	if got := escapeTag("deepseek-chat"); got != `deepseek\-chat` {
		t.Errorf("连字符应转义: %q", got)
	}
	if got := escapeTag("gpt_4o"); got != "gpt_4o" {
		t.Errorf("字母数字下划线不转义: %q", got)
	}
}

func TestNewSemanticBadOptions(t *testing.T) {
	e := &fakeEmbedder{}
	if _, err := NewSemantic(SemanticOptions{Addr: "x", TTL: 0, Threshold: 0.95, Dimensions: 8, Embedder: e}); err == nil {
		t.Error("TTL=0 应报错")
	}
	if _, err := NewSemantic(SemanticOptions{Addr: "x", TTL: time.Hour, Threshold: 1.5, Dimensions: 8, Embedder: e}); err == nil {
		t.Error("threshold>1 应报错")
	}
	if _, err := NewSemantic(SemanticOptions{Addr: "x", TTL: time.Hour, Threshold: 0.95, Dimensions: 8, Embedder: nil}); err == nil {
		t.Error("nil embedder 应报错")
	}
}

// TestSemanticDimensionSelfHeal 模拟更换 embedding 模型维度：
// 已存在 8 维索引时以 16 维重建客户端，应探测出不匹配并 DROP 重建自愈。
func TestSemanticDimensionSelfHeal(t *testing.T) {
	addr := "127.0.0.1:6380"
	ctx := context.Background()

	e8 := &fakeEmbedder{vecs: map[string][]float32{"问题": unitVec(8, 0)}}
	s1, err := NewSemantic(SemanticOptions{Addr: addr, TTL: time.Hour, Threshold: 0.95, Dimensions: 8, Embedder: e8})
	if err != nil {
		t.Skipf("redis-stack 不可达（%v），跳过", err)
	}
	if err := s1.Store(ctx, "m", "问题", e8.vecs["问题"], []byte(`{"r":1}`)); err != nil {
		t.Fatalf("Store 失败: %v", err)
	}
	s1.Close()
	t.Cleanup(func() {
		c := redis.NewClient(&redis.Options{Addr: addr})
		c.Do(ctx, "FT.DROPINDEX", semanticIndex)
		c.Del(ctx, semanticKey("m", "问题"))
		c.Close()
	})

	// 维度切换到 16：旧索引不可用，NewSemantic 应自愈重建
	e16 := &fakeEmbedder{vecs: map[string][]float32{"问题": unitVec(16, 0)}}
	s2, err := NewSemantic(SemanticOptions{Addr: addr, TTL: time.Hour, Threshold: 0.95, Dimensions: 16, Embedder: e16})
	if err != nil {
		t.Fatalf("维度漂移后 NewSemantic 应自愈，实际报错: %v", err)
	}
	defer s2.Close()

	// 新索引下 Lookup/Store 正常
	hit, vec, err := s2.Lookup(ctx, "m", "问题")
	if err != nil {
		t.Fatalf("自愈后 Lookup 失败: %v", err)
	}
	if hit != nil {
		t.Logf("旧 8 维条目不兼容 16 维索引（预期 miss 或跳过）")
	}
	if err := s2.Store(ctx, "m", "问题", vec, []byte(`{"r":2}`)); err != nil {
		t.Fatalf("自愈后 Store 失败: %v", err)
	}
}

// TestSemanticIntegration 真实 embedding + redis-stack 端到端（需 EMBEDDING_API_KEY）。
// 同义改写（实测余弦 0.996）应命中阈值 0.95 的语义缓存。
func TestSemanticIntegration(t *testing.T) {
	key := os.Getenv("EMBEDDING_API_KEY")
	if key == "" {
		t.Skip("无 EMBEDDING_API_KEY，跳过真实调用")
	}
	embedder := embedding.New(embedding.Options{
		BaseURL:    "https://api.siliconflow.cn/v1",
		APIKey:     key,
		Model:      "BAAI/bge-m3",
		Dimensions: 1024,
	})
	// 真实模型是 1024 维，不能用 newTestSemantic 的 testDims，单独构造
	s, err := NewSemantic(SemanticOptions{
		Addr: "127.0.0.1:6380", TTL: time.Hour, Threshold: 0.95,
		Dimensions: 1024, Embedder: embedder,
	})
	if err != nil {
		t.Skipf("redis-stack 不可达（%v），跳过集成测试", err)
	}
	defer func() {
		s.client.Do(context.Background(), "FT.DROPINDEX", semanticIndex)
		s.Close()
	}()
	ctx := context.Background()

	original := "user: 如何快速提升英语听力\n"
	rewritten := "user: 怎样迅速提高英语听力水平\n"

	body := []byte(`{"id":"ri","choices":[{"message":{"content":"每天精听 30 分钟"}}]}`)
	vec, err := s.embedder.Embed(ctx, original)
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if err := s.Store(ctx, "test-model", original, vec, body); err != nil {
		t.Fatalf("Store 失败: %v", err)
	}
	t.Cleanup(func() { s.client.Del(ctx, semanticKey("test-model", original)) })

	hit, _, err := s.Lookup(ctx, "test-model", rewritten)
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if string(hit) != string(body) {
		t.Errorf("真实同义改写应命中语义缓存，实际 hit=%s", hit)
	}
}
