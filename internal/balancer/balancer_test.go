package balancer

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewWeightedEmpty(t *testing.T) {
	if w := NewWeighted[string](nil, nil); w != nil {
		t.Fatal("empty nodes should return nil")
	}
}

func TestSingleNode(t *testing.T) {
	w := NewWeighted([]string{"a"}, []int{5})
	for i := 0; i < 10; i++ {
		if got := w.Next(); got != "a" {
			t.Fatalf("pick %d = %q, want %q", i, got, "a")
		}
	}
}

// 等权重退化为严格轮询
func TestEqualWeightsRoundRobin(t *testing.T) {
	w := NewWeighted([]string{"a", "b", "c"}, []int{1, 1, 1})
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, name := range want {
		if got := w.Next(); got != name {
			t.Fatalf("pick %d = %q, want %q", i, got, name)
		}
	}
}

// 3:1 权重应输出平滑序列 AABA，而非简单轮询的 AAAB
func TestWeightedSmoothSequence(t *testing.T) {
	w := NewWeighted([]string{"a", "b"}, []int{3, 1})
	want := []string{"a", "a", "b", "a"}
	for i, name := range want {
		if got := w.Next(); got != name {
			t.Fatalf("pick %d = %q, want %q", i, got, name)
		}
	}
}

// 整数个周期后分布必须严格等于权重比
func TestWeightedDistribution(t *testing.T) {
	w := NewWeighted([]string{"a", "b", "c"}, []int{5, 3, 2})
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[w.Next()]++
	}
	if counts["a"] != 50 || counts["b"] != 30 || counts["c"] != 20 {
		t.Fatalf("counts = %v, want a=50 b=30 c=20", counts)
	}
}

// 权重缺省、0、负值均按 1 处理
func TestDefaultWeightIsOne(t *testing.T) {
	w := NewWeighted([]string{"a", "b"}, nil)
	if got := w.Next(); got != "a" {
		t.Fatalf("pick = %q, want a", got)
	}
	if got := w.Next(); got != "b" {
		t.Fatalf("pick = %q, want b", got)
	}

	w2 := NewWeighted([]string{"a", "b"}, []int{0, -3})
	if got := w2.Next(); got != "a" {
		t.Fatalf("pick = %q, want a", got)
	}
	if got := w2.Next(); got != "b" {
		t.Fatalf("pick = %q, want b", got)
	}
}

// 并发下总选择数仍严格符合权重比（互斥锁保证状态串行推进）
func TestConcurrentDistribution(t *testing.T) {
	w := NewWeighted([]string{"a", "b"}, []int{3, 1})
	const goroutines = 8
	const perGoroutine = 1000

	var aCount, bCount atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if w.Next() == "a" {
					aCount.Add(1)
				} else {
					bCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	total := int64(goroutines * perGoroutine)
	if aCount.Load() != total*3/4 || bCount.Load() != total/4 {
		t.Fatalf("a=%d b=%d, want a=%d b=%d",
			aCount.Load(), bCount.Load(), total*3/4, total/4)
	}
}
