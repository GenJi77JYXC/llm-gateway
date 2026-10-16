package breaker

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 共享熔断器的 Redis 键前缀、本地缓存有效期与 Redis 操作超时。
const (
	sharedKeyPrefix = "llmgw:breaker:"
	remoteCacheTTL  = 500 * time.Millisecond
	sharedOpTimeout = 500 * time.Millisecond
)

// SharedOptions 是 Shared 的构造参数。
type SharedOptions struct {
	// Name 上游实例名，用于 Redis 键（同一上游的全部网关在实例共享一个打开状态）
	Name        string
	MaxFailures int
	Cooldown    time.Duration

	Addr     string
	Password string
	DB       int
}

// Shared 是多实例共享的熔断器：本地三态状态机（内嵌 Breaker）不变，
// 本地新跳闸时经 Redis 广播打开状态，全体实例 500ms 内感知并拒绝请求；
// 打开状态由 Redis TTL 天然驱动，到期自动消失即全员进入半开（本地半开逻辑接管）。
//
// 热路径零 Redis 调用：远端状态查询结果（含 Redis 故障）本地缓存 500ms；
// OnSuccess 纯本地；仅跳闸瞬间一次 SET。Redis 故障时静默退化为纯本地熔断器。
type Shared struct {
	local    *Breaker
	rdb      *redis.Client
	openKey  string
	cooldown time.Duration

	mu         sync.Mutex
	remoteOpen bool      // 远端打开状态的本地缓存
	cacheUntil time.Time // 缓存截止时刻
	now        func() time.Time
}

// NewShared 创建共享熔断器并 Ping 探测；连不上返回错误，由调用方降级为内存版。
func NewShared(opts SharedOptions) (*Shared, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  sharedOpTimeout,
		WriteTimeout: sharedOpTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second+sharedOpTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	cooldown := opts.Cooldown
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}
	return &Shared{
		local:    New(opts.MaxFailures, opts.Cooldown),
		rdb:      rdb,
		openKey:  sharedKeyPrefix + opts.Name + ":open",
		cooldown: cooldown,
		now:      time.Now,
	}, nil
}

// Allow 报告当前是否放行请求：远端打开则全员拒绝，否则走本地状态机。
func (s *Shared) Allow() bool {
	if s.isRemoteOpen() {
		return false
	}
	return s.local.Allow()
}

// OnSuccess 上报成功：纯本地操作（远端打开状态等 Redis TTL 自然到期，不主动清除，
// 避免上游抖动时全体实例在打开/闭合间快速翻转）。
func (s *Shared) OnSuccess() {
	s.local.OnSuccess()
}

// OnFailure 上报失败：本地状态机新跳闸时广播打开状态（含半开探测失败重新跳闸）。
func (s *Shared) OnFailure() {
	wasOpen := s.local.State() == StateOpen
	s.local.OnFailure()
	if !wasOpen && s.local.State() == StateOpen {
		s.broadcast()
	}
}

// State 返回当前状态（远端打开优先，供指标暴露时全体实例显示一致）。
func (s *Shared) State() State {
	if s.isRemoteOpen() {
		return StateOpen
	}
	return s.local.State()
}

// isRemoteOpen 查询远端打开状态；结果（含 Redis 故障）缓存 500ms，
// 热路径绝大多数调用零 Redis 往返。Redis 故障视为远端闭合，退化为纯本地熔断。
func (s *Shared) isRemoteOpen() bool {
	s.mu.Lock()
	if s.now().Before(s.cacheUntil) {
		open := s.remoteOpen
		s.mu.Unlock()
		return open
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), sharedOpTimeout)
	n, err := s.rdb.Exists(ctx, s.openKey).Result()
	cancel()

	open := err == nil && n > 0
	s.mu.Lock()
	s.remoteOpen = open
	s.cacheUntil = s.now().Add(remoteCacheTTL)
	s.mu.Unlock()
	return open
}

// broadcast 广播打开状态：SET open key，TTL = 冷却期，到期自动消失即全员半开。
// 写入失败静默（仅本实例熔断），下次跳闸会再尝试。
func (s *Shared) broadcast() {
	ctx, cancel := context.WithTimeout(context.Background(), sharedOpTimeout)
	defer cancel()
	_ = s.rdb.Set(ctx, s.openKey, 1, s.cooldown).Err()

	// 本实例立即感知，不必等 500ms 缓存过期
	s.mu.Lock()
	s.remoteOpen = true
	s.cacheUntil = s.now().Add(remoteCacheTTL)
	s.mu.Unlock()
}

// Close 关闭底层 Redis 连接。
func (s *Shared) Close() error {
	return s.rdb.Close()
}
