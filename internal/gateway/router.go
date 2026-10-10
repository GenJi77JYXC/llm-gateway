// Package gateway 是网关的核心业务层：模型路由与请求归一化。
// 不依赖任何 HTTP 框架，API 层只负责 HTTP <-> 领域类型的翻译。
package gateway

import (
	"fmt"
	"sort"

	"github.com/GenJi77JYXC/llm-gateway/internal/provider"
)

// Router 按 model 字段选择 Provider。
// Week 1：一个模型只对应一个上游（config 校验已保证）；
// Week 2 将在此引入候选列表 + 负载均衡/熔断，对外接口保持不变。
type Router struct {
	byModel map[string]provider.Provider
	byName  map[string]provider.Provider
	models  []string // 排好序，保证 /v1/models 输出稳定
}

// NewRouter 构造路由器。
func NewRouter(providers []provider.Provider) *Router {
	r := &Router{
		byModel: make(map[string]provider.Provider, len(providers)),
		byName:  make(map[string]provider.Provider, len(providers)),
	}
	for _, p := range providers {
		r.byName[p.Name()] = p
		for _, m := range p.Models() {
			r.byModel[m] = p
			r.models = append(r.models, m)
		}
	}
	sort.Strings(r.models)
	return r
}

// Pick 按模型名选择 Provider，未找到返回 ErrModelNotFound。
func (r *Router) Pick(model string) (provider.Provider, error) {
	if p, ok := r.byModel[model]; ok {
		return p, nil
	}
	return nil, &ErrModelNotFound{Model: model}
}

// Models 返回所有已注册模型名（有序）。
func (r *Router) Models() []string {
	out := make([]string, len(r.models))
	copy(out, r.models)
	return out
}

// Owner 返回模型所属的 Provider 名，未知模型返回 "unknown"。
func (r *Router) Owner(model string) string {
	if p, ok := r.byModel[model]; ok {
		return p.Name()
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
