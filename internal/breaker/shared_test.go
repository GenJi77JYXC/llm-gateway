package breaker

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestShared 创建连接 miniredis 的共享熔断器，并注入可手动推进的时钟
//（本地状态机与远端缓存共用一个时钟，保证状态转移可确定性验证）。
func newTestShared(t *testing.T, mr *miniredis.Miniredis, name string, maxFailures int, cooldown time.Duration) (*Shared, *fakeClock) {
	t.Helper()
	s, err := NewShared(SharedOptions{
		Name:        name,
		MaxFailures: maxFailures,
		Cooldown:    cooldown,
		Addr:        mr.Addr(),
	})
	if err != nil {
		t.Fatalf("创建 Shared 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	clk := &fakeClock{now: time.Unix(1000, 0)}
	s.now = clk.Now
	s.local.now = clk.Now
	return s, clk
}

func sharedOpenKey(name string) string {
	return sharedKeyPrefix + name + ":open"
}

// 连续失败达阈值跳闸时，open key 广播到 Redis 且 TTL 为冷却期
func TestSharedBroadcastsTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	s, _ := newTestShared(t, mr, "up-a", 3, 30*time.Second)

	s.OnFailure()
	s.OnFailure()
	if mr.Exists(sharedOpenKey("up-a")) {
		t.Fatal("未达阈值不应广播")
	}
	s.OnFailure()
	if !mr.Exists(sharedOpenKey("up-a")) {
		t.Fatal("跳闸后应广播 open key")
	}
	if ttl := mr.TTL(sharedOpenKey("up-a")); ttl <= 0 || ttl > 30*time.Second {
		t.Fatalf("open key TTL 应为冷却期 30s 内: %v", ttl)
	}
}

// 实例 A 跳闸后，实例 B（本地无失败记录）经 Redis 感知并拒绝
func TestSharedRemoteOpenBlocksOtherInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	a, _ := newTestShared(t, mr, "up-a", 3, 30*time.Second)
	b, _ := newTestShared(t, mr, "up-a", 3, 30*time.Second)

	a.OnFailure()
	a.OnFailure()
	a.OnFailure()

	if b.Allow() {
		t.Fatal("远端打开时其他实例应拒绝")
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("远端打开时 State 应报告 Open: %v", got)
	}
}

// 远端状态本地缓存 500ms：缓存期内 key 消失仍按旧值，过期后重新查询
func TestSharedRemoteStateCached(t *testing.T) {
	mr := miniredis.RunT(t)
	b, clk := newTestShared(t, mr, "up-a", 3, 30*time.Second)

	if err := mr.Set(sharedOpenKey("up-a"), "1"); err != nil {
		t.Fatal(err)
	}
	mr.SetTTL(sharedOpenKey("up-a"), 30*time.Second)

	if b.Allow() {
		t.Fatal("远端 open 应拒绝")
	}
	mr.FastForward(31 * time.Second) // Redis 中 key 已过期

	// 本地缓存未过期：仍按缓存的 open 拒绝
	if b.Allow() {
		t.Fatal("缓存期内应继续按缓存值拒绝")
	}
	// 缓存过期：重新查询发现 key 消失，放行
	clk.Advance(remoteCacheTTL + time.Millisecond)
	if !b.Allow() {
		t.Fatal("缓存过期后应重新查询并放行")
	}
}

// 远端 open TTL 到期后，本地冷却同时结束的实例进入半开；
// 探测失败立即重新跳闸并再次广播
func TestSharedHalfOpenProbeFailureRebroadcasts(t *testing.T) {
	mr := miniredis.RunT(t)
	s, clk := newTestShared(t, mr, "up-a", 1, 30*time.Second)

	s.OnFailure() // 阈值 1，跳闸并广播
	if !mr.Exists(sharedOpenKey("up-a")) {
		t.Fatal("跳闸应广播")
	}

	mr.FastForward(31 * time.Second) // 远端 key 过期
	clk.Advance(31 * time.Second)    // 本地冷却到期 + 缓存过期

	if !s.Allow() {
		t.Fatal("冷却后半开应放行探测")
	}
	s.OnFailure() // 探测失败：重新跳闸
	if !mr.Exists(sharedOpenKey("up-a")) {
		t.Fatal("半开探测失败应重新广播 open key")
	}
	if s.Allow() {
		t.Fatal("重新打开后应拒绝")
	}
}

// Redis 连接失败：NewShared 返回错误（由调用方降级内存版）
func TestSharedConnectFailure(t *testing.T) {
	_, err := NewShared(SharedOptions{Addr: "127.0.0.1:1", Name: "x"})
	if err == nil {
		t.Fatal("连不上 Redis 应返回错误")
	}
}

// 运行中 Redis 故障：静默退化为纯本地熔断器
func TestSharedDegradesWhenRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	s, _ := newTestShared(t, mr, "up-a", 3, 30*time.Second)
	mr.Close()

	// 本地闭合 + Redis 查询失败视为远端闭合：放行
	if !s.Allow() {
		t.Fatal("Redis 故障应退化为本地熔断（闭合放行）")
	}
	// 本地跳闸后 broadcast 失败静默，本地打开状态仍然生效
	s.OnFailure()
	s.OnFailure()
	s.OnFailure()
	if s.Allow() {
		t.Fatal("本地打开应拒绝（broadcast 失败不影响本地状态机）")
	}
	if got := s.State(); got != StateOpen {
		t.Fatalf("本地打开时 State 应报告 Open: %v", got)
	}
}
