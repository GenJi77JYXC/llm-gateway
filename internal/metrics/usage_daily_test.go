package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeDailySource 按日期返回预置数据，err 非空时模拟 Redis 故障。
type fakeDailySource struct {
	data map[string]int64
	err  error
}

func (f fakeDailySource) DailyUsage(_ context.Context, _ time.Time) (map[string]int64, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

// collectAll 收集全部序列，返回 "date|key_id|kind|model" → value。
func collectAll(t *testing.T, c *UsageDailyCollector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			k := labels["date"] + "|" + labels["key_id"] + "|" + labels["kind"] + "|" + labels["model"]
			out[k] = m.GetGauge().GetValue()
		}
	}
	return out
}

func TestUsageDailyCollector(t *testing.T) {
	c := NewUsageDailyCollector(fakeDailySource{data: map[string]int64{
		"dev|deepseek-chat|prompt":     19,
		"dev|deepseek-chat|completion": 6,
		"dev|deepseek-chat|total":      25,
		"badfield":                     99, // 非三段格式应被跳过
	}})

	got := collectAll(t, c)
	today := time.Now().Format("2006-01-02")

	// fake 不区分日期，7 天每天 3 条合法序列
	if len(got) != 7*3 {
		t.Fatalf("期望 21 条序列（7 天 × 3 kind，badfield 跳过），实际 %d", len(got))
	}
	want := map[string]float64{
		today + "|dev|prompt|deepseek-chat":     19,
		today + "|dev|completion|deepseek-chat": 6,
		today + "|dev|total|deepseek-chat":      25,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("序列 %s 期望 %v，实际 %v", k, v, got[k])
		}
	}
}

func TestUsageDailyCollectorRedisDown(t *testing.T) {
	c := NewUsageDailyCollector(fakeDailySource{err: errors.New("connection refused")})

	// Redis 故障时应产出零序列而不是报错（不阻塞 /metrics）
	if got := collectAll(t, c); len(got) != 0 {
		t.Errorf("Redis 故障时不应有序列，实际 %d 条", len(got))
	}
}
