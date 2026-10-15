package usage

import (
	"sync"
	"testing"
	"time"
)

func TestMemRecordAggregate(t *testing.T) {
	m := NewMem()
	m.Record(Event{KeyID: "dev", Model: "deepseek-chat", PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30})
	m.Record(Event{KeyID: "dev", Model: "deepseek-chat", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	m.Record(Event{KeyID: "ci", Model: "deepseek-chat", PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10})
	m.Record(Event{KeyID: "dev", Model: "gpt-4o", PromptTokens: 7, CompletionTokens: 8, TotalTokens: 15})

	got := m.Snapshot()
	if len(got) != 3 {
		t.Fatalf("期望聚合出 3 组，实际 %d 组: %+v", len(got), got)
	}
	// Snapshot 按 KeyID/Model/Day 排序：ci < dev
	if got[0].KeyID != "ci" {
		t.Errorf("排序后第一组应为 ci，实际 %s", got[0].KeyID)
	}
	devChat := got[1]
	if devChat.KeyID != "dev" || devChat.Model != "deepseek-chat" {
		t.Errorf("第二组应为 dev/deepseek-chat，实际 %s/%s", devChat.KeyID, devChat.Model)
	}
	if devChat.PromptTokens != 11 || devChat.CompletionTokens != 22 || devChat.TotalTokens != 33 {
		t.Errorf("dev/deepseek-chat 累加错误: %+v", devChat)
	}
	if devChat.Day != time.Now().Format(dayLayout) {
		t.Errorf("Day 应为本地日期 %s，实际 %s", time.Now().Format(dayLayout), devChat.Day)
	}
}

func TestMemConcurrentRecord(t *testing.T) {
	m := NewMem()
	const goroutines, perG = 50, 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				m.Record(Event{KeyID: "k", Model: "m", PromptTokens: 2, CompletionTokens: 0, TotalTokens: 2})
			}
		}()
	}
	wg.Wait()

	got := m.Snapshot()
	if len(got) != 1 {
		t.Fatalf("期望 1 组，实际 %d 组", len(got))
	}
	if want := int64(goroutines * perG * 2); got[0].PromptTokens != want {
		t.Errorf("并发累加丢失: 期望 prompt=%d，实际 %d", want, got[0].PromptTokens)
	}
}
