// Package balancer 实现平滑加权轮询（smooth weighted round-robin）负载均衡。
// 算法与 Nginx SWRR 相同：每轮所有节点 current += weight，取 current 最大者，
// 选中节点 current -= totalWeight。相比简单加权轮询（AAAB），
// 平滑版本输出更均匀（AABA），避免流量瞬时打满单个上游。
// 本包不依赖 provider/gateway 等任何业务包，通过泛型复用。
package balancer

import "sync"

// Weighted 平滑加权轮询选择器，并发安全。
type Weighted[T any] struct {
	mu      sync.Mutex
	nodes   []T
	weights []int // 配置权重
	current []int // 动态权重
	total   int   // 权重之和
}

// NewWeighted 构造选择器。nodes 与 weights 一一对应；
// weights 长度不足或对应值 <= 0 时按 1 处理。
// nodes 为空返回 nil，调用方需判空。
func NewWeighted[T any](nodes []T, weights []int) *Weighted[T] {
	if len(nodes) == 0 {
		return nil
	}
	w := &Weighted[T]{
		nodes:   nodes,
		weights: make([]int, len(nodes)),
		current: make([]int, len(nodes)),
	}
	for i := range nodes {
		wt := 1
		if i < len(weights) && weights[i] > 0 {
			wt = weights[i]
		}
		w.weights[i] = wt
		w.total += wt
	}
	return w
}

// Next 按平滑加权轮询选出下一个节点。
func (w *Weighted[T]) Next() T {
	w.mu.Lock()
	defer w.mu.Unlock()

	best := 0
	for i := range w.nodes {
		w.current[i] += w.weights[i]
		if w.current[i] > w.current[best] {
			best = i
		}
	}
	w.current[best] -= w.total
	return w.nodes[best]
}

// Len 返回节点数。
func (w *Weighted[T]) Len() int {
	return len(w.nodes)
}
