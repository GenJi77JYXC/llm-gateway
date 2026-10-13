package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// fakeProvider 实现 provider.Provider，仅用于路由测试（不会真的发请求）。
type fakeProvider struct {
	name   string
	models []string
}

func (f *fakeProvider) Name() string     { return f.name }
func (f *fakeProvider) Models() []string { return f.models }

func (f *fakeProvider) Chat(context.Context, *provider.ChatRequest) (*provider.Result, error) {
	return nil, fmt.Errorf("fakeProvider: Chat not implemented")
}

func (f *fakeProvider) ChatStream(context.Context, *provider.ChatRequest) (provider.Stream, error) {
	return nil, fmt.Errorf("fakeProvider: ChatStream not implemented")
}

// 同一 model 配在两个上游（3:1）时，Pick 按平滑加权轮询分发
func TestPickDistributesByWeight(t *testing.T) {
	a := &fakeProvider{name: "a", models: []string{"m"}}
	b := &fakeProvider{name: "b", models: []string{"m"}}
	r := NewRouter([]Upstream{
		{Provider: a, Weight: 3},
		{Provider: b, Weight: 1},
	})

	want := []string{"a", "a", "b", "a"} // 平滑序列 AABA
	for i, name := range want {
		p, err := r.Pick("m")
		if err != nil {
			t.Fatalf("pick %d: unexpected error: %v", i, err)
		}
		if p.Name() != name {
			t.Fatalf("pick %d = %q, want %q", i, p.Name(), name)
		}
	}
}

// 权重缺省（0）按 1 处理，两个上游等权轮询
func TestPickDefaultWeight(t *testing.T) {
	a := &fakeProvider{name: "a", models: []string{"m"}}
	b := &fakeProvider{name: "b", models: []string{"m"}}
	r := NewRouter([]Upstream{{Provider: a}, {Provider: b}})

	for i, name := range []string{"a", "b", "a", "b"} {
		p, err := r.Pick("m")
		if err != nil {
			t.Fatalf("pick %d: unexpected error: %v", i, err)
		}
		if p.Name() != name {
			t.Fatalf("pick %d = %q, want %q", i, p.Name(), name)
		}
	}
}

func TestPickModelNotFound(t *testing.T) {
	r := NewRouter([]Upstream{
		{Provider: &fakeProvider{name: "a", models: []string{"m"}}, Weight: 1},
	})
	_, err := r.Pick("nonexistent")
	var notFound *ErrModelNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
	if notFound.Model != "nonexistent" {
		t.Fatalf("notFound.Model = %q, want %q", notFound.Model, "nonexistent")
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
