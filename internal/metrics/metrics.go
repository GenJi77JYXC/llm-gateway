// Package metrics 集中管理 Prometheus 指标。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "llmgateway"

var (
	// RequestsTotal 请求计数。
	// result: ok / upstream_error / bad_request / internal_error
	// stream: true / false
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "requests_total",
		Help:      "Total number of chat completion requests handled.",
	}, []string{"provider", "model", "stream", "result"})

	// RequestDuration 端到端请求耗时（非流式为完整响应时间，流式为 [DONE] 收尾时间）。
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "request_duration_seconds",
		Help:      "End-to-end request duration in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"provider", "model", "stream"})

	// TTFT 流式首 token 延迟（Time To First Token）。
	TTFT = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "ttft_seconds",
		Help:      "Time to first SSE data chunk in seconds (stream only).",
		Buckets:   []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30},
	}, []string{"provider", "model"})

	// TokensTotal 上游返回的 token 用量。kind: prompt / completion / total
	TokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "tokens_total",
		Help:      "Tokens reported by upstream usage.",
	}, []string{"provider", "model", "kind"})

	// InflightRequests 当前在途请求数。
	InflightRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "inflight_requests",
		Help:      "Current in-flight chat completion requests.",
	}, []string{"stream"})

	// StreamInterruptTotal 上游流中途异常断开次数（正常 EOF 不计）。
	StreamInterruptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "stream_interrupt_total",
		Help:      "Upstream SSE streams ended with an error before [DONE].",
	}, []string{"provider", "model"})
)

// StreamLabel 将 bool 转为指标 label，避免调用处散落字符串。
func StreamLabel(stream bool) string {
	if stream {
		return "true"
	}
	return "false"
}
