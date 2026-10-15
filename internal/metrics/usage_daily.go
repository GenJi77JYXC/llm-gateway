package metrics

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// usageDailyDays 是每日计量指标回溯的天数（与 Redis hash 8 天 TTL 匹配，留 1 天余量）。
const usageDailyDays = 7

// DailyUsageSource 提供按日聚合的 token 用量（由 usage.RedisRecorder 实现）。
// 返回 map 的键格式为 {keyID}|{model}|{kind}。
type DailyUsageSource interface {
	DailyUsage(ctx context.Context, day time.Time) (map[string]int64, error)
}

// UsageDailyCollector 在每次 Prometheus scrape 时从 Redis 现查最近 N 天的
// 计量聚合，暴露 llmgateway_usage_daily_tokens{key_id,model,kind,date}。
//
// 设计取舍：不用 Counter 而每次现查，是因为数据权威在 Redis（HINCRBY 聚合），
// 进程重启、多实例（未来）都不会丢数；scrape 频率（秒级）对 Redis 压力可忽略。
// Redis 故障时返回空序列，不影响 /metrics 其他指标。
type UsageDailyCollector struct {
	source DailyUsageSource
	desc   *prometheus.Desc
}

// NewUsageDailyCollector 创建按日计量 collector（需手动 prometheus.MustRegister）。
func NewUsageDailyCollector(source DailyUsageSource) *UsageDailyCollector {
	return &UsageDailyCollector{
		source: source,
		desc: prometheus.NewDesc(
			namespace+"_usage_daily_tokens",
			"Daily aggregated tokens per client key and model, read from Redis at scrape time.",
			[]string{"key_id", "model", "kind", "date"}, nil,
		),
	}
}

// Describe 实现 prometheus.Collector。
func (c *UsageDailyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect 实现 prometheus.Collector：逐天 HGETALL，解析 field 为 key_id|model|kind。
func (c *UsageDailyCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	now := time.Now()
	for i := 0; i < usageDailyDays; i++ {
		day := now.AddDate(0, 0, -i)
		fields, err := c.source.DailyUsage(ctx, day)
		if err != nil {
			continue // Redis 故障静默跳过当日，不阻塞 /metrics
		}
		date := day.Format("2006-01-02")
		for field, val := range fields {
			parts := strings.SplitN(field, "|", 3)
			if len(parts) != 3 {
				continue
			}
			ch <- prometheus.MustNewConstMetric(
				c.desc, prometheus.GaugeValue, float64(val),
				parts[0], parts[1], parts[2], date,
			)
		}
	}
}
