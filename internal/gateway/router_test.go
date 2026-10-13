package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// fakeProvider 实现 provider.Provider，仅用于路由测试（不会真的发请求）。
// chatFn/streamFn 可编程每次调用的行为，nil 时返回默认成功。
type fakeProvider struct {
	name    string
	models  []string
	chatFn  func() (*provider.Result, error)
	chatN   int // Chat 调用次数（测试均为单线程）
	streamN int // ChatStream 调用次数
}

func (f *fakeProvider) Name() string     { return f.name }
func (f *fakeProvider) Models() []string { return f.models }

func (f *fakeProvider) Chat(context.Context, *provider.ChatRequest) (*provider.Result, error) {
	f.chatN++
	if f.chatFn != nil {
		return f.chatFn()
	}
	return &provider.Result{StatusCode: 200}, nil
}

func (f *fakeProvider) ChatStream(context.Context, *provider.ChatRequest) (provider.Stream, error) {
	f.streamN++
	return nil, fmt.Errorf("fakeProvider: ChatStream not implemented")
}

func TestChatModelNotFound(t *testing.T) {
	r := NewRouter([]Upstream{
		{Provider: &fakeProvider{name: "a", models: []string{"m"}}, Weight: 1},
	})
	_, _, err := r.Chat(context.Background(), "nonexistent", &provider.ChatRequest{})
	var notFound *ErrModelNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
	if notFound.Model != "nonexistent" {
		t.Fatalf("notFound.Model = %q, want %q", notFound.Model, "nonexistent")
	}
}

// 单候选模型：Chat 始终命中该上游
func TestChatSingleUpstream(t *testing.T) {
	a := &fakeProvider{name: "a", models: []string{"m"}}
	r := NewRouter([]Upstream{{Provider: a, Weight: 1}})

	for i := 0; i < 3; i++ {
		_, name, err := r.Chat(context.Background(), "m", &provider.ChatRequest{})
		if err != nil {
			t.Fatalf("chat %d: unexpected error: %v", i, err)
		}
		if name != "a" {
			t.Fatalf("chat %d provider = %q, want a", i, name)
		}
	}
	if a.chatN != 3 {
		t.Fatalf("a.chatN = %d, want 3", a.chatN)
	}
}

// 多上游时 Owner 返回首个注册的 provider（仅展示用途），Models 输出稳定有序
func TestOwnerAndModels(t *testing.T) {
	r := NewRouter([]Upstream{
		{Provider: &fakeProvider{name: "b", models: []string{"m2", "m1"}}, Weight: 1},
		{Provider: &fakeProvider{name: "a", models: []string{"m1"}}, Weight: 1},
	})

	if got := r.Owner("m1"); got != "b" {
		t.Fatalf("Owner(m1) = %q, want %q（首个注册者）", got, "b")
	}
	if got := r.Owner("nonexistent"); got != "unknown" {
		t.Fatalf("Owner(nonexistent) = %q, want unknown", got)
	}

	models := r.Models()
	if len(models) != 2 || models[0] != "m1" || models[1] != "m2" {
		t.Fatalf("Models() = %v, want [m1 m2]", models)
	}
}
