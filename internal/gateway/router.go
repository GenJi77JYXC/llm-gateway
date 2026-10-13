// Package gateway 是网关的核心业务层：模型路由与请求归一化。
// 不依赖任何 HTTP 框架，API 层只负责 HTTP <-> 领域类型的翻译。
package gateway

import (
	"fmt"
	"sort"

	"github.com/GenJi77JYXC/llm-gateway/internal/balancer"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// Upstream 是某个模型的一个候选上游：Provider 实例 + 配置权重。
type Upstream struct {
	Provider provider.Provider
	// Weight 负载均衡权重，<=0 时按 1 处理
	Weight int
}

// Router 按 model 字段选择 Provider。
// Week 2：一个模型可对应多个候选上游，按平滑加权轮询分发；
// 后续将在此引入熔断候选过滤 + fallback，对外接口保持不变。
type Router struct {
	byModel map[string]*balancer.Weighted[provider.Provider]
	owners  map[string]string // model -> 首个注册的 provider 名（/v1/models 展示用）
	models  []string          // 排好序，保证 /v1/models 输出稳定
}

// NewRouter 构造路由器。同一模型的多个上游按 weight 加权轮询。
func NewRouter(upstreams []Upstream) *Router {
	r := &Router{
		byModel: make(map[string]*balancer.Weighted[provider.Provider]),
		owners:  make(map[string]string),
	}

	// 按 model 分组收集候选（保持配置顺序，首个注册者作为 owner）
	candidates := make(map[string][]provider.Provider)
	weights := make(map[string][]int)
	for _, u := range upstreams {
		for _, m := range u.Provider.Models() {
			if _, ok := candidates[m]; !ok {
				r.models = append(r.models, m)
				r.owners[m] = u.Provider.Name()
			}
			candidates[m] = append(candidates[m], u.Provider)
			weights[m] = append(weights[m], u.Weight)
		}
	}
	for m, ps := range candidates {
		r.byModel[m] = balancer.NewWeighted(ps, weights[m])
	}
	sort.Strings(r.models)
	return r
}

// Pick 按模型名加权轮询选出一个 Provider，未找到返回 ErrModelNotFound。
// 每次调用都可能返回不同上游（按权重轮转），调用方不应缓存结果。
func (r *Router) Pick(model string) (provider.Provider, error) {
	if w, ok := r.byModel[model]; ok {
		return w.Next(), nil
	}
	return nil, &ErrModelNotFound{Model: model}
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
