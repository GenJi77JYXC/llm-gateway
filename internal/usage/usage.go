// Package usage 负责 chat 调用的 token 计量：每次成功调用产生一条计量事件，
// 按 key_id + model + 日期 维度聚合，是成本看板与配额控制的数据源。
// 计量是尽力而为组件：实现必须非阻塞，事件失败静默丢弃，不得影响转发主链路。
package usage

import (
	"sort"
	"sync"
	"time"
)

// Event 是一次成功调用的计量事件。tokens 取自上游返回的 usage
// （流式为末尾 usage chunk，非流式为响应体 usage 字段），网关不做本地估算。
type Event struct {
	KeyID            string // 客户端身份标识，即 auth.client_keys.name
	Model            string // 客户端请求的模型名
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Recorder 是计量事件记录器。实现自行承担异步聚合职责，
// Record 调用方不感知失败（计量缺失可接受，转发失败不可接受）。
type Recorder interface {
	Record(ev Event)
}

// dayLayout 聚合维度的日期格式（本地时区）。
const dayLayout = "2006-01-02"

// Entry 是一组 key_id + model + 日期 的聚合结果。
type Entry struct {
	KeyID            string
	Model            string
	Day              string // 日期 YYYY-MM-DD
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// aggKey 是聚合 map 的键。
type aggKey struct {
	keyID, model, day string
}

// Mem 是内存聚合 Recorder：按 key_id+model+日期 累加，进程重启即丢。
// w3-2 引入 Redis 聚合后，本实现保留用于测试与降级兜底。
type Mem struct {
	mu      sync.Mutex
	entries map[aggKey]*Entry
}

// NewMem 创建内存聚合器。
func NewMem() *Mem {
	return &Mem{entries: make(map[aggKey]*Entry)}
}

// Record 累加一条计量事件（并发安全），按收到事件的本地日期归档。
func (m *Mem) Record(ev Event) {
	day := time.Now().Format(dayLayout)
	k := aggKey{keyID: ev.KeyID, model: ev.Model, day: day}

	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[k]
	if !ok {
		e = &Entry{KeyID: k.keyID, Model: k.model, Day: k.day}
		m.entries[k] = e
	}
	e.PromptTokens += int64(ev.PromptTokens)
	e.CompletionTokens += int64(ev.CompletionTokens)
	e.TotalTokens += int64(ev.TotalTokens)
}

// Snapshot 返回当前聚合结果（按 key/model/day 排序），用于观测与测试。
func (m *Mem) Snapshot() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].KeyID != out[j].KeyID {
			return out[i].KeyID < out[j].KeyID
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Day < out[j].Day
	})
	return out
}
