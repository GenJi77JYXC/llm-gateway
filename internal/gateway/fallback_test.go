package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/GenJi77JYXC/llm-gateway/internal/metrics"
	"github.com/GenJi77JYXC/llm-gateway/internal/model"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// fakeStream 实现 provider.Stream，建立即视为成功。
type fakeStream struct{}

func (fakeStream) StatusCode() int                           { return http.StatusOK }
func (fakeStream) Header() http.Header                       { return http.Header{} }
func (fakeStream) Next() ([]byte, *model.Usage, bool, error) { return nil, nil, true, io.EOF }
func (fakeStream) Close() error                              { return nil }

// streamFailProvider 是 ChatStream 建立阶段失败的 fakeProvider。
type streamFailProvider struct {
	fakeProvider
	streamFn func() (provider.Stream, error)
	streamN  int
}

func (f *streamFailProvider) ChatStream(context.Context, *provider.ChatRequest) (provider.Stream, error) {
	f.streamN++
	return f.streamFn()
}

func upstreamErr(status int) error {
	return &provider.UpstreamError{StatusCode: status, Body: []byte(`{"error":"x"}`)}
}

// 首选上游 5xx → 自动 fallback 到备选，结果来自备选，并计入 fallback 指标
func TestChatFallbackOn5xx(t *testing.T) {
	a := &fakeProvider{name: "fb5-a", models: []string{"m5"}, chatFn: func() (*provider.Result, error) {
		return nil, upstreamErr(500)
	}}
	b := &fakeProvider{name: "fb5-b", models: []string{"m5"}}
	r := newRouter([]Upstream{{Provider: a, Weight: 1}, {Provider: b, Weight: 1}}, 3, time.Minute)

	res, name, err := r.Chat(context.Background(), "m5", &provider.ChatRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "fb5-b" || res.StatusCode != 200 {
		t.Fatalf("provider = %q status = %d, want fb5-b/200（fallback 到备选）", name, res.StatusCode)
	}
	if a.chatN != 1 || b.chatN != 1 {
		t.Fatalf("a.chatN = %d b.chatN = %d, want 1/1", a.chatN, b.chatN)
	}
	if got := testutil.ToFloat64(metrics.FallbackTotal.WithLabelValues("m5", "fb5-a")); got != 1 {
		t.Fatalf("fallback_total{m5,fb5-a} = %v, want 1", got)
	}
}

// 上游 4xx 是请求本身的问题：不 fallback、不计熔断失败
func TestChatNoFallbackOn4xx(t *testing.T) {
	a := &fakeProvider{name: "fb4-a", models: []string{"m4"}, chatFn: func() (*provider.Result, error) {
		return nil, upstreamErr(400)
	}}
	b := &fakeProvider{name: "fb4-b", models: []string{"m4"}}
	r := newRouter([]Upstream{{Provider: a, Weight: 1}, {Provider: b, Weight: 1}}, 1, time.Minute)

	_, name, err := r.Chat(context.Background(), "m4", &provider.ChatRequest{})
	var ue *provider.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 400 {
		t.Fatalf("err = %v, want UpstreamError 400 原样返回", err)
	}
	if name != "fb4-a" {
		t.Fatalf("provider = %q, want fb4-a（未发生 fallback）", name)
	}
	if b.chatN != 0 {
		t.Fatalf("b.chatN = %d, want 0（4xx 不重试）", b.chatN)
	}

	// 单独验证 4xx 未计入熔断：阈值 1 下若计失败 a 必熔断，
	// 单候选模型第二次调用应返回 400 而非 ErrAllUpstreamDown
	solo := &fakeProvider{name: "fb4-solo", models: []string{"m4s"}, chatFn: func() (*provider.Result, error) {
		return nil, upstreamErr(400)
	}}
	r2 := newRouter([]Upstream{{Provider: solo, Weight: 1}}, 1, time.Minute)
	_, _, _ = r2.Chat(context.Background(), "m4s", &provider.ChatRequest{})
	_, _, err = r2.Chat(context.Background(), "m4s", &provider.ChatRequest{})
	var allDown *ErrAllUpstreamDown
	if errors.As(err, &allDown) {
		t.Fatal("err = ErrAllUpstreamDown，说明 4xx 被错误计入了熔断")
	}
	if solo.chatN != 2 {
		t.Fatalf("solo.chatN = %d, want 2（4xx 不计入熔断）", solo.chatN)
	}
}

// 全部候选持续 5xx：先透传最后的上游错误，熔断全部打开后返回 ErrAllUpstreamDown
func TestChatAllUpstreamDown(t *testing.T) {
	a := &fakeProvider{name: "fbd-a", models: []string{"md"}, chatFn: func() (*provider.Result, error) {
		return nil, upstreamErr(500)
	}}
	b := &fakeProvider{name: "fbd-b", models: []string{"md"}, chatFn: func() (*provider.Result, error) {
		return nil, upstreamErr(500)
	}}
	// 阈值 2：第 2 次请求后两个候选都熔断
	r := newRouter([]Upstream{{Provider: a, Weight: 1}, {Provider: b, Weight: 1}}, 2, time.Minute)

	for i := 0; i < 2; i++ {
		_, _, err := r.Chat(context.Background(), "md", &provider.ChatRequest{})
		var ue *provider.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 500 {
			t.Fatalf("round %d: err = %v, want UpstreamError 500", i, err)
		}
	}
	if a.chatN != 2 || b.chatN != 2 {
		t.Fatalf("a.chatN = %d b.chatN = %d, want 2/2", a.chatN, b.chatN)
	}

	_, _, err := r.Chat(context.Background(), "md", &provider.ChatRequest{})
	var allDown *ErrAllUpstreamDown
	if !errors.As(err, &allDown) {
		t.Fatalf("err = %v, want ErrAllUpstreamDown（全部熔断）", err)
	}
	if a.chatN != 2 || b.chatN != 2 {
		t.Fatalf("熔断后仍发起请求: a.chatN = %d b.chatN = %d, want 2/2", a.chatN, b.chatN)
	}
}

// 首选熔断打开后，后续请求只走健康候选（fail-fast，不再打故障上游）
func TestChatSkipsOpenBreaker(t *testing.T) {
	a := &fakeProvider{name: "fbs-a", models: []string{"ms"}, chatFn: func() (*provider.Result, error) {
		return nil, upstreamErr(500)
	}}
	b := &fakeProvider{name: "fbs-b", models: []string{"ms"}}
	// 阈值 1：a 失败一次即熔断
	r := newRouter([]Upstream{{Provider: a, Weight: 1}, {Provider: b, Weight: 1}}, 1, time.Minute)

	for i := 0; i < 3; i++ {
		_, name, err := r.Chat(context.Background(), "ms", &provider.ChatRequest{})
		if err != nil {
			t.Fatalf("round %d: unexpected error: %v", i, err)
		}
		if name != "fbs-b" {
			t.Fatalf("round %d: provider = %q, want fbs-b", i, name)
		}
	}
	if a.chatN != 1 {
		t.Fatalf("a.chatN = %d, want 1（熔断后未再打扰 a）", a.chatN)
	}
	if b.chatN != 3 {
		t.Fatalf("b.chatN = %d, want 3", b.chatN)
	}
}

// 调用方 ctx 已取消：错误不污染熔断器、不 fallback
func TestChatClientCancelNotCounted(t *testing.T) {
	a := &fakeProvider{name: "fbc-a", models: []string{"mc"}, chatFn: func() (*provider.Result, error) {
		return nil, context.Canceled
	}}
	b := &fakeProvider{name: "fbc-b", models: []string{"mc"}}
	// 阈值 1：若被计失败 a 必熔断
	r := newRouter([]Upstream{{Provider: a, Weight: 1}, {Provider: b, Weight: 1}}, 1, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := r.Chat(ctx, "mc", &provider.ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if b.chatN != 0 {
		t.Fatalf("b.chatN = %d, want 0（ctx 取消不 fallback）", b.chatN)
	}

	// 单独验证取消未计入熔断：阈值 1 下若计失败 a 必熔断，
	// 单候选模型第二次调用应能打到 a 而非 ErrAllUpstreamDown
	solo := &fakeProvider{name: "fbc-solo", models: []string{"mcs"}, chatFn: func() (*provider.Result, error) {
		return nil, context.Canceled
	}}
	r2 := newRouter([]Upstream{{Provider: solo, Weight: 1}}, 1, time.Minute)
	_, _, _ = r2.Chat(ctx, "mcs", &provider.ChatRequest{})
	_, _, err = r2.Chat(context.Background(), "mcs", &provider.ChatRequest{})
	var allDown *ErrAllUpstreamDown
	if errors.As(err, &allDown) {
		t.Fatal("err = ErrAllUpstreamDown，说明 ctx 取消被错误计入了熔断")
	}
	if solo.chatN != 2 {
		t.Fatalf("solo.chatN = %d, want 2（ctx 取消未计入熔断）", solo.chatN)
	}
}

// 流式：首选建立阶段 5xx → fallback 到备选；备选建立成功后不再重试
func TestChatStreamFallbackOnEstablishFailure(t *testing.T) {
	a := &streamFailProvider{
		fakeProvider: fakeProvider{name: "fss-a", models: []string{"mst"}},
		streamFn: func() (provider.Stream, error) {
			return nil, upstreamErr(500)
		},
	}
	b := &streamFailProvider{
		fakeProvider: fakeProvider{name: "fss-b", models: []string{"mst"}},
		streamFn: func() (provider.Stream, error) {
			return fakeStream{}, nil
		},
	}
	r := newRouter([]Upstream{{Provider: a, Weight: 1}, {Provider: b, Weight: 1}}, 3, time.Minute)

	stream, name, err := r.ChatStream(context.Background(), "mst", &provider.ChatRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stream.Close()
	if name != "fss-b" {
		t.Fatalf("provider = %q, want fss-b（fallback 到备选）", name)
	}
	if a.streamN != 1 || b.streamN != 1 {
		t.Fatalf("a.streamN = %d b.streamN = %d, want 1/1", a.streamN, b.streamN)
	}
}
