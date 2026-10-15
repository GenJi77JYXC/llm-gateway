package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/GenJi77JYXC/llm-gateway/internal/config"
	"github.com/GenJi77JYXC/llm-gateway/internal/gateway"
	"github.com/GenJi77JYXC/llm-gateway/internal/model"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
	"github.com/GenJi77JYXC/llm-gateway/internal/usage"
)

// fakeProvider 返回固定 usage 的假上游。
type fakeProvider struct{}

func (f *fakeProvider) Name() string     { return "fake" }
func (f *fakeProvider) Models() []string { return []string{"test-model"} }

func (f *fakeProvider) Chat(context.Context, *provider.ChatRequest) (*provider.Result, error) {
	return &provider.Result{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		Usage:      &model.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}, nil
}

func (f *fakeProvider) ChatStream(context.Context, *provider.ChatRequest) (provider.Stream, error) {
	return &fakeStream{chunks: []fakeChunk{
		{raw: []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)},
		{raw: []byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
			usage: &model.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}},
		{done: true},
	}}, nil
}

// fakeStream 按预置 chunk 序列回放 SSE。
type fakeStream struct {
	chunks []fakeChunk
	i      int
}

type fakeChunk struct {
	raw   []byte
	usage *model.Usage
	done  bool
}

func (s *fakeStream) StatusCode() int     { return http.StatusOK }
func (s *fakeStream) Header() http.Header { return http.Header{} }
func (s *fakeStream) Close() error        { return nil }
func (s *fakeStream) Next() ([]byte, *model.Usage, bool, error) {
	if s.i >= len(s.chunks) {
		return nil, nil, false, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c.raw, c.usage, c.done, nil
}

// collectRecorder 收集计量事件供断言。
type collectRecorder struct{ evts []usage.Event }

func (r *collectRecorder) Record(ev usage.Event) { r.evts = append(r.evts, ev) }

// setup 组装带鉴权与计量器的测试引擎。
func setup(t *testing.T, rec *collectRecorder) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	r := gateway.NewRouter([]gateway.Upstream{{Provider: &fakeProvider{}, Weight: 1}})
	NewHandler(r, rec).RegisterRoutes(engine,
		[]config.ClientKey{{Name: "dev", Key: "sk-test"}}, nil)
	return engine
}

func postChat(engine *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)
	return resp
}

func TestChatCompletionsBlockingRecordsUsage(t *testing.T) {
	rec := &collectRecorder{}
	engine := setup(t, rec)

	resp := postChat(engine, `{"model":"test-model"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", resp.Code, resp.Body.String())
	}
	if len(rec.evts) != 1 {
		t.Fatalf("期望 1 条计量事件，实际 %d", len(rec.evts))
	}
	ev := rec.evts[0]
	want := usage.Event{KeyID: "dev", Model: "test-model", PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	if ev != want {
		t.Errorf("计量事件不符\n期望 %+v\n实际 %+v", want, ev)
	}
}

func TestChatCompletionsStreamRecordsUsage(t *testing.T) {
	rec := &collectRecorder{}
	engine := setup(t, rec)

	resp := postChat(engine, `{"model":"test-model","stream":true}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "data: [DONE]") {
		t.Errorf("流式响应应含 [DONE] 收尾，实际: %s", resp.Body.String())
	}
	if len(rec.evts) != 1 {
		t.Fatalf("期望 1 条计量事件，实际 %d", len(rec.evts))
	}
	ev := rec.evts[0]
	want := usage.Event{KeyID: "dev", Model: "test-model", PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	if ev != want {
		t.Errorf("计量事件不符\n期望 %+v\n实际 %+v", want, ev)
	}
}

func TestChatCompletionsBadKeyNotRecorded(t *testing.T) {
	rec := &collectRecorder{}
	engine := setup(t, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", resp.Code)
	}
	if len(rec.evts) != 0 {
		t.Errorf("鉴权失败不应产生计量事件，实际 %d 条", len(rec.evts))
	}
}
