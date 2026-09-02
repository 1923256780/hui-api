package billing

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// newLogTestStore 打开临时库并迁移（日志落库验证用）。
func newLogTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/log-test.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return st
}

func countLogs(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var n int64
	if err := st.Read.Model(&model.Log{}).Count(&n).Error; err != nil {
		t.Fatalf("统计日志失败: %v", err)
	}
	return n
}

func sampleRecord(i int) LogRecord {
	return LogRecord{
		UserID:           1,
		TokenID:          2,
		ChannelID:        3,
		Protocol:         "openai",
		ModelName:        "m-log",
		PromptTokens:     100 + i,
		CompletionTokens: 20 + i,
		Quota:            int64(7 * i),
		UseTime:          2,
		IsStream:         i%2 == 0,
		CreatedTime:      time.Now().Unix(),
		Detail: Detail{
			Mode:      string(ModeTieredExpr),
			Expr:      `tier("base", p * 0.15 + c * 0.5)`,
			Frozen:    30,
			CacheRead: 5,
			BilledIn:  95,
		},
	}
}

// TestAsyncLogWriterBatchAndDrain 批量落库 + Close 排空：全部记录最终可见。
func TestAsyncLogWriterBatchAndDrain(t *testing.T) {
	st := newLogTestStore(t)
	w := NewAsyncLogWriterWith(st, LogWriterConfig{Buffer: 128, Batch: 8, Interval: 50 * time.Millisecond})

	const total = 100
	for i := 0; i < total; i++ {
		w.Submit(sampleRecord(i))
	}
	w.Close(3 * time.Second)

	if got := countLogs(t, st); got != total {
		t.Fatalf("排空后应落库 %d 条，实际 %d", total, got)
	}
	if w.Dropped() != 0 {
		t.Fatalf("不应有丢弃，实际 %d", w.Dropped())
	}

	// 落库内容抽验：detail JSON 反向可读（对账依据）。
	var row model.Log
	if err := st.Read.Where("model_name = ?", "m-log").First(&row).Error; err != nil {
		t.Fatalf("读取日志行失败: %v", err)
	}
	if row.Detail == "" {
		t.Fatal("detail 不应为空")
	}
	var d Detail
	if err := json.Unmarshal([]byte(row.Detail), &d); err != nil {
		t.Fatalf("detail 应为合法 JSON: %v (%s)", err, row.Detail)
	}
	if d.Mode != string(ModeTieredExpr) || d.Frozen != 30 || d.CacheRead != 5 {
		t.Fatalf("detail 内容不符: %+v", d)
	}
}

// TestAsyncLogWriterTimedFlush 攒批窗口到期即落库（不依赖 Close）。
func TestAsyncLogWriterTimedFlush(t *testing.T) {
	st := newLogTestStore(t)
	w := NewAsyncLogWriterWith(st, LogWriterConfig{Buffer: 16, Batch: 1024, Interval: 30 * time.Millisecond})
	w.Submit(sampleRecord(1))
	w.Submit(sampleRecord(2))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countLogs(t, st) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("定时窗口应触发落库，实际 %d 条", countLogs(t, st))
}

// TestAsyncLogWriterDropWhenFull 队列满时 Submit 非阻塞（超时即失败）。
// 丢弃计数本身以「停机后 Submit」确定性验证（见 CloseIdempotent）。
func TestAsyncLogWriterDropWhenFull(t *testing.T) {
	st := newLogTestStore(t)
	w := NewAsyncLogWriterWith(st, LogWriterConfig{Buffer: 1, Batch: 1024, Interval: time.Hour})
	defer w.Close(time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			w.Submit(sampleRecord(i)) // 超出缓冲的部分被丢弃，且不阻塞
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit 在队列满时阻塞了")
	}
}

// TestAsyncLogWriterCloseIdempotent Close 幂等；停机后 Submit 确定性丢弃计数。
func TestAsyncLogWriterCloseIdempotent(t *testing.T) {
	st := newLogTestStore(t)
	w := NewAsyncLogWriterWith(st, LogWriterConfig{Buffer: 8, Batch: 4, Interval: 20 * time.Millisecond})
	w.Submit(sampleRecord(1))
	w.Close(time.Second)
	w.Close(time.Second) // 二次调用不 panic
	for i := 0; i < 3; i++ {
		w.Submit(sampleRecord(2 + i))
	}
	if got := w.Dropped(); got != 3 {
		t.Fatalf("停机后 Submit 应确定性丢弃计数，期望 3，实际 %d", got)
	}
	if got := countLogs(t, st); got != 1 {
		t.Fatalf("停机后 Submit 应丢弃，落库应为 1 条，实际 %d", got)
	}
}

// TestAsyncLogWriterConcurrentSubmit 并发提交不丢不重（-race）。
func TestAsyncLogWriterConcurrentSubmit(t *testing.T) {
	st := newLogTestStore(t)
	w := NewAsyncLogWriterWith(st, LogWriterConfig{Buffer: 512, Batch: 16, Interval: 20 * time.Millisecond})

	const perG, goroutines = 50, 8
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				w.Submit(sampleRecord(g*perG + i))
			}
		}()
	}
	wg.Wait()
	w.Close(3 * time.Second)

	if got := countLogs(t, st); got != perG*goroutines {
		t.Fatalf("并发提交应全部落库 %d 条，实际 %d", perG*goroutines, got)
	}
}

// TestDetailEncode 全零 detail 编码为空串；非零可逆。
func TestDetailEncode(t *testing.T) {
	if got := (Detail{}).encode(); got != "" {
		t.Fatalf("全零 detail 应为空串，实际 %q", got)
	}
	d := Detail{Mode: "per_call", Frozen: 9}
	var back Detail
	if err := json.Unmarshal([]byte(d.encode()), &back); err != nil {
		t.Fatalf("detail 编解码失败: %v", err)
	}
	if back != d {
		t.Fatalf("detail 编解码不一致: %+v vs %+v", back, d)
	}
}
