package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 可手动推进的时钟，替代 time.Now 让状态转移可确定性验证。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestBreaker(maxFailures int, cooldown time.Duration) (*Breaker, *fakeClock) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	b := New(maxFailures, cooldown)
	b.now = clk.Now
	return b, clk
}

// 闭合状态正常放行，连续失败达到阈值跳闸
func TestClosedTripsAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)

	for i := 0; i < 2; i++ {
		if !b.Allow() {
			t.Fatalf("failure %d: closed state should allow", i+1)
		}
		b.OnFailure()
		if b.State() != StateClosed {
			t.Fatalf("failure %d: state = %s, want closed（未达阈值）", i+1, b.State())
		}
	}

	b.OnFailure() // 第 3 次连续失败，跳闸
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open", b.State())
	}
	if b.Allow() {
		t.Fatal("open state should reject")
	}
}

// 中途一次成功会重置连续失败计数，不应跳闸
func TestSuccessResetsFailureCount(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)

	b.OnFailure()
	b.OnFailure()
	b.OnSuccess() // 重置计数
	b.OnFailure()
	b.OnFailure()
	if b.State() != StateClosed {
		t.Fatalf("state = %s, want closed（失败被成功打断，累计不跳闸）", b.State())
	}
}

// 打开状态冷却期过后惰性转入半开，且只放行一个探测
func TestOpenToHalfOpenAfterCooldown(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.OnFailure() // 阈值 1，直接跳闸

	if b.Allow() {
		t.Fatal("冷却期内应拒绝")
	}

	clk.Advance(time.Minute + time.Second)
	if !b.Allow() {
		t.Fatal("冷却期后应放行探测请求")
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %s, want half_open", b.State())
	}
	if b.Allow() {
		t.Fatal("半开状态已有探测在途，应拒绝其他请求")
	}
}

// 半开探测成功 → 闭合恢复，后续请求全部放行
func TestHalfOpenProbeSuccessCloses(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.OnFailure()
	clk.Advance(2 * time.Minute)

	if !b.Allow() {
		t.Fatal("冷却期后应放行探测")
	}
	b.OnSuccess()
	if b.State() != StateClosed {
		t.Fatalf("state = %s, want closed（探测成功恢复）", b.State())
	}
	if !b.Allow() || !b.Allow() {
		t.Fatal("闭合后应全部放行")
	}
}

// 半开探测失败 → 重新打开，开始新的冷却期
func TestHalfOpenProbeFailureReopens(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.OnFailure()
	clk.Advance(2 * time.Minute)

	b.Allow()      // 探测
	b.OnFailure() // 探测失败
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open（探测失败重新跳闸）", b.State())
	}
	if b.Allow() {
		t.Fatal("重新打开后冷却期内应拒绝")
	}

	clk.Advance(2 * time.Minute)
	if !b.Allow() {
		t.Fatal("第二个冷却期后应再次放行探测")
	}
}

// 跳闸前发出的请求，跳闸后才返回失败：属"旧账"，不延长冷却期
func TestStaleFailureDoesNotExtendCooldown(t *testing.T) {
	b, clk := newTestBreaker(2, time.Minute)
	b.Allow()      // 请求 1 在途
	b.OnFailure() // 请求 2 失败，failures=1
	b.OnFailure() // 请求 3 失败，跳闸，冷却至 t+60s

	b.OnFailure() // 请求 1 此刻才返回失败（state 已是 open）
	clk.Advance(time.Minute + time.Second)
	if !b.Allow() {
		t.Fatal("冷却期不应被旧请求的失败延长")
	}
}

func TestStateString(t *testing.T) {
	if StateClosed.String() != "closed" ||
		StateOpen.String() != "open" ||
		StateHalfOpen.String() != "half_open" {
		t.Fatalf("unexpected state strings: %s %s %s",
			StateClosed, StateOpen, StateHalfOpen)
	}
}

func TestNewNormalization(t *testing.T) {
	b := New(0, 0)
	if b.maxFailures != 1 {
		t.Fatalf("maxFailures = %d, want 1", b.maxFailures)
	}
	if b.cooldown != 30*time.Second {
		t.Fatalf("cooldown = %v, want 30s", b.cooldown)
	}
}

// 并发下半开状态最多放行一个探测（互斥锁串行保证，结果确定）
func TestHalfOpenSingleProbeConcurrent(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.OnFailure()
	clk.Advance(2 * time.Minute)

	const n = 16
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 1 {
		t.Fatalf("半开状态放行 %d 个请求, want 1", allowed.Load())
	}
}
