// Package gateway 是网关的核心业务层：模型路由与请求归一化。
// 不依赖任何 HTTP 框架，API 层只负责 HTTP <-> 领域类型的翻译。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/GenJi77JYXC/llm-gateway/internal/balancer"
	"github.com/GenJi77JYXC/llm-gateway/internal/breaker"
	"github.com/GenJi77JYXC/llm-gateway/internal/metrics"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// 熔断策略默认值（暂未开放配置）：同一上游连续失败 3 次熔断，冷却 30s 后半开探测。
const (
	defaultMaxFailures = 3
	defaultCooldown    = 30 * time.Second
)

// Upstream 是某个模型的一个候选上游：Provider 实例 + 配置权重。
type Upstream struct {
	Provider provider.Provider
	// Weight 负载均衡权重，<=0 时按 1 处理
	Weight int
}

// candidate 是路由内部候选：上游 + 熔断器（同一上游实例跨 model 共享一个熔断器）。
type candidate struct {
	p  provider.Provider
	br *breaker.Breaker
}

// modelRoute 是一个模型的路由：加权负载均衡 + 完整候选列表（fallback 遍历用）。
type modelRoute struct {
	bal   *balancer.Weighted[*candidate]
	cands []*candidate
}

// Router 按 model 字段路由请求。
// Week 2：多上游加权轮询 + 熔断过滤 + 首字节前失败 fallback。
type Router struct {
	byModel map[string]*modelRoute
	owners  map[string]string // model -> 首个注册的 provider 名（/v1/models 展示用）
	models  []string          // 排好序，保证 /v1/models 输出稳定
}

// NewRouter 构造路由器（使用默认熔断策略）。
func NewRouter(upstreams []Upstream) *Router {
	return newRouter(upstreams, defaultMaxFailures, defaultCooldown)
}

// newRouter 允许测试注入熔断策略。
func newRouter(upstreams []Upstream, maxFailures int, cooldown time.Duration) *Router {
	r := &Router{
		byModel: make(map[string]*modelRoute),
		owners:  make(map[string]string),
	}

	// 每个上游实例一个熔断器，跨其所有 model 共享
	breakers := make([]*breaker.Breaker, len(upstreams))
	for i := range upstreams {
		breakers[i] = breaker.New(maxFailures, cooldown)
	}

	candidates := make(map[string][]*candidate)
	weights := make(map[string][]int)
	for i, u := range upstreams {
		c := &candidate{p: u.Provider, br: breakers[i]}
		for _, m := range u.Provider.Models() {
			if _, ok := candidates[m]; !ok {
				r.models = append(r.models, m)
				r.owners[m] = u.Provider.Name()
			}
			candidates[m] = append(candidates[m], c)
			weights[m] = append(weights[m], u.Weight)
		}
	}
	for m, cs := range candidates {
		r.byModel[m] = &modelRoute{
			bal:   balancer.NewWeighted(cs, weights[m]),
			cands: cs,
		}
	}
	sort.Strings(r.models)
	return r
}

// Chat 非流式调用，带熔断过滤与首字节前 fallback。
// 返回最终命中的 provider 名（指标 label 用）；
// 模型未注册返回 ErrModelNotFound，全部候选不可用返回 ErrAllUpstreamDown。
func (r *Router) Chat(ctx context.Context, model string, req *provider.ChatRequest) (*provider.Result, string, error) {
	rt, ok := r.byModel[model]
	if !ok {
		return nil, "", &ErrModelNotFound{Model: model}
	}

	tried := make(map[string]bool, len(rt.cands))
	var lastErr error
	lastName := ""
	for {
		c := rt.pick(tried)
		if c == nil {
			if lastErr != nil {
				return nil, lastName, lastErr
			}
			return nil, "", &ErrAllUpstreamDown{Model: model}
		}
		tried[c.p.Name()] = true
		if !c.br.Allow() {
			// 熔断中：未发起请求，直接换下一个候选（不计 fallback 指标）
			metrics.SetBreakerState(c.p.Name(), c.br.State())
			continue
		}
		metrics.SetBreakerState(c.p.Name(), c.br.State())

		res, err := c.p.Chat(ctx, req)
		ok, retry := classify(ctx, err)
		if ok {
			c.br.OnSuccess()
			metrics.SetBreakerState(c.p.Name(), c.br.State())
		}
		if !retry {
			return res, c.p.Name(), err
		}
		c.br.OnFailure()
		metrics.SetBreakerState(c.p.Name(), c.br.State())
		metrics.FallbackTotal.WithLabelValues(model, c.p.Name()).Inc()
		lastErr = err
		lastName = c.p.Name()
	}
}

// ChatStream 建立流式调用，带熔断过滤与首字节前 fallback。
// 拿到上游 2xx 响应头即算成功（OnSuccess）；流建立后中途失败不可重试，
// 由调用方按 interrupted 处理，且不计入熔断（可能是客户端断开）。
func (r *Router) ChatStream(ctx context.Context, model string, req *provider.ChatRequest) (provider.Stream, string, error) {
	rt, ok := r.byModel[model]
	if !ok {
		return nil, "", &ErrModelNotFound{Model: model}
	}

	tried := make(map[string]bool, len(rt.cands))
	var lastErr error
	lastName := ""
	for {
		c := rt.pick(tried)
		if c == nil {
			if lastErr != nil {
				return nil, lastName, lastErr
			}
			return nil, "", &ErrAllUpstreamDown{Model: model}
		}
		tried[c.p.Name()] = true
		if !c.br.Allow() {
			metrics.SetBreakerState(c.p.Name(), c.br.State())
			continue
		}
		metrics.SetBreakerState(c.p.Name(), c.br.State())

		stream, err := c.p.ChatStream(ctx, req)
		ok, retry := classify(ctx, err)
		if ok {
			c.br.OnSuccess()
			metrics.SetBreakerState(c.p.Name(), c.br.State())
		}
		if !retry {
			return stream, c.p.Name(), err
		}
		c.br.OnFailure()
		metrics.SetBreakerState(c.p.Name(), c.br.State())
		metrics.FallbackTotal.WithLabelValues(model, c.p.Name()).Inc()
		lastErr = err
		lastName = c.p.Name()
	}
}

// pick 选出下一个未尝试过的候选。权重悬殊时平滑轮询短期内可能重复命中
// 同一节点，去重 n 次后线性兜底，保证 fallback 路径能遍历全部候选。
func (rt *modelRoute) pick(tried map[string]bool) *candidate {
	for i := 0; i < len(rt.cands); i++ {
		if c := rt.bal.Next(); !tried[c.p.Name()] {
			return c
		}
	}
	for _, c := range rt.cands {
		if !tried[c.p.Name()] {
			return c
		}
	}
	return nil
}

// classify 判定一次调用结果如何影响熔断器、以及是否可 fallback：
//   - 成功：上报成功
//   - 调用方 ctx 已终结（客户端断开）：错误大概率由客户端引起，不污染熔断器，不重试
//   - UpstreamError 4xx：上游可达，是请求本身的问题，算成功（重置连输），重试无意义
//   - 其余（5xx、连接拒绝、DNS、超时等）：算失败，fallback 到下一个候选
func classify(ctx context.Context, err error) (reportSuccess, canRetry bool) {
	switch {
	case err == nil:
		return true, false
	case ctx.Err() != nil:
		return false, false
	}
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.StatusCode < 500 {
		return true, false
	}
	return false, true
}

// Models 返回所有已注册模型名（有序）。
func (r *Router) Models() []string {
	out := make([]string, len(r.models))
	copy(out, r.models)
	return out
}

// Owner 返回模型首个注册的 Provider 名，未知模型返回 "unknown"。
// 一个模型有多个上游时仅作展示用途，不代表实际路由结果。
func (r *Router) Owner(model string) string {
	if name, ok := r.owners[model]; ok {
		return name
	}
	return "unknown"
}

// ErrModelNotFound 表示客户端请求的模型未在任何上游配置中。
type ErrModelNotFound struct {
	Model string
}

func (e *ErrModelNotFound) Error() string {
	return fmt.Sprintf("model not supported: %s", e.Model)
}

// ErrAllUpstreamDown 表示模型的所有候选上游均不可用（全部熔断中）。
type ErrAllUpstreamDown struct {
	Model string
}

func (e *ErrAllUpstreamDown) Error() string {
	return fmt.Sprintf("all upstreams for model %s are unavailable", e.Model)
}
