// Package breaker 实现三态熔断器（闭合/打开/半开），上游故障时快速失败。
//
// 状态机：
//   - 闭合（Closed）：正常放行；连续失败达到阈值后跳闸进入打开
//   - 打开（Open）：直接拒绝（fail-fast），冷却期过后进入半开
//   - 半开（HalfOpen）：只放行一个探测请求；成功则闭合恢复，失败则重新打开
//
// 状态转移采用惰性求值（Allow/State 时检查冷却期），无需后台定时器。
// 什么算"失败"由调用方决定：建议传输层错误与上游 5xx 计为失败，4xx 不计。
// 本包不依赖任何业务包；注入 now 时钟（测试用）保证状态转移可确定性验证。
package breaker

import (
	"sync"
	"time"
)

// State 是熔断器三态。
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

// 默认熔断策略：同一上游连续失败 3 次熔断，冷却 30s 后半开探测。
// 由 gateway 与 main 装配共享，避免两处各写一份魔法数。
const (
	DefaultMaxFailures = 3
	DefaultCooldown    = 30 * time.Second
)

// Interface 是熔断器抽象：进程内内存版（Breaker）与多实例共享版（Shared）均实现。
// 调用方（gateway 路由）只依赖此接口，不关心状态是否经 Redis 广播。
type Interface interface {
	Allow() bool
	OnSuccess()
	OnFailure()
	State() State
}

// String 返回状态名，用于日志与指标 label。
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// Breaker 是并发安全的三态熔断器。
type Breaker struct {
	mu          sync.Mutex
	state       State
	failures    int           // 当前连续失败次数
	maxFailures int           // 跳闸阈值（连续失败次数）
	cooldown    time.Duration // 打开状态冷却期
	openUntil   time.Time     // 冷却截止时刻
	probing     bool          // 半开状态下是否已有探测请求在途
	now         func() time.Time
}

// New 构造熔断器。maxFailures < 1 按 1 处理，cooldown <= 0 按 30s 处理。
func New(maxFailures int, cooldown time.Duration) *Breaker {
	if maxFailures < 1 {
		maxFailures = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{
		maxFailures: maxFailures,
		cooldown:    cooldown,
		now:         time.Now,
	}
}

// Allow 报告当前是否放行请求。
// 打开状态下冷却期过后惰性转入半开，当前请求成为探测；
// 半开状态只放行一个探测，其余请求拒绝（由调用方转投其他候选）。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if b.now().Before(b.openUntil) {
			return false
		}
		b.state = StateHalfOpen
		b.probing = true
		return true
	default: // StateHalfOpen
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// OnSuccess 上报一次成功：半开探测成功则闭合恢复；闭合状态下重置连续失败计数。
func (b *Breaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateHalfOpen {
		b.state = StateClosed
		b.probing = false
	}
	b.failures = 0
}

// OnFailure 上报一次失败：闭合下累计连续失败，达到阈值跳闸；
// 半开探测失败立即重新打开并开始新的冷却期。
func (b *Breaker) OnFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		b.probing = false
		b.trip()
	case StateOpen:
		// 请求在跳闸前发出、跳闸后才返回失败：属于"旧账"，不延长冷却期
	default:
		b.failures++
		if b.failures >= b.maxFailures {
			b.trip()
		}
	}
}

// trip 跳闸进入打开状态并开始冷却计时。
func (b *Breaker) trip() {
	b.state = StateOpen
	b.failures = 0
	b.openUntil = b.now().Add(b.cooldown)
}

// State 返回当前状态（与 Allow 同样的惰性转移，用于指标暴露）。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && !b.now().Before(b.openUntil) {
		b.state = StateHalfOpen
	}
	return b.state
}
