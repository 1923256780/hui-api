// worker_test.go 后台任务池与订单超时关单测试（M3-wave4）：
// 池生命周期（按周期执行/Stop 幂等且等待退出/Stop 后 Start 不生效）、panic
// 隔离（panic 任务不带崩进程且后续 tick 照常执行）、非法任务忽略；关单语义
// （过期 pending→expired、未到期 pending 不动、已支付单永不关单、幂等重跑）。
package worker

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// newTestStore 打开临时库并迁移。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return st
}

// orderSeq 测试订单号唯一化序号（Windows 时钟粒度下 UnixNano 可能重复）。
var orderSeq atomic.Int64

// seedOrder 写一笔指定状态/创建时间偏移的充值订单并返回。
func seedOrder(t *testing.T, st *store.Store, status int, createdOffset time.Duration) model.TopupOrder {
	t.Helper()
	o := model.TopupOrder{
		OrderNo:     fmt.Sprintf("TPWK%d-%d", time.Now().UnixNano(), orderSeq.Add(1)),
		Gateway:     "epay",
		Currency:    "CNY",
		AmountCents: 100,
		Status:      status,
		CreatedTime: time.Now().Add(createdOffset).Unix(),
	}
	if err := st.Write.Create(&o).Error; err != nil {
		t.Fatalf("写入订单失败: %v", err)
	}
	return o
}

// TestPoolRunsTasksAndStops 池生命周期：任务按周期执行、Stop 幂等且等待退出、
// Stop 后任务不再执行、Stop 后 Start 不生效。
func TestPoolRunsTasksAndStops(t *testing.T) {
	var n atomic.Int64
	p := NewPool()
	p.Start(Task{
		Name:     "counter",
		Interval: 5 * time.Millisecond,
		Run:      func() { n.Add(1) },
	})
	time.Sleep(60 * time.Millisecond)
	if n.Load() == 0 {
		t.Fatal("任务应已执行至少一次")
	}
	p.Stop()
	p.Stop() // 幂等
	after := n.Load()
	time.Sleep(30 * time.Millisecond)
	if n.Load() != after {
		t.Fatalf("Stop 后任务不应再执行: before=%d after=%d", after, n.Load())
	}
	p.Start(Task{Name: "late", Interval: time.Millisecond, Run: func() { n.Add(1) }})
	time.Sleep(20 * time.Millisecond)
	if n.Load() != after {
		t.Fatalf("Stop 后 Start 不应生效: before=%d after=%d", after, n.Load())
	}
}

// TestPoolRecoversPanic panic 隔离：panic 不带崩进程，同一任务后续 tick 照常执行。
func TestPoolRecoversPanic(t *testing.T) {
	var n atomic.Int64
	p := NewPool()
	p.Start(Task{
		Name:     "panicky",
		Interval: 5 * time.Millisecond,
		Run:      func() { n.Add(1); panic("boom") },
	})
	time.Sleep(80 * time.Millisecond)
	p.Stop()
	if n.Load() < 2 {
		t.Fatalf("panic 后任务应继续执行，实际执行 %d 次", n.Load())
	}
}

// TestPoolIgnoresInvalidTasks 非法任务（无执行体/非法周期）被忽略且不阻塞 Stop。
func TestPoolIgnoresInvalidTasks(t *testing.T) {
	p := NewPool()
	p.Start(
		Task{Name: "nil-run", Interval: time.Millisecond},
		Task{Name: "bad-interval", Interval: 0, Run: func() {}},
	)
	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 不应被非法任务阻塞")
	}
}

// TestExpireStaleTopupOrders 关单语义：过期 pending→expired（status=4）、
// 未到期 pending 不动、已支付单永不关单、幂等重跑迁移 0 行、默认阈值兜底。
func TestExpireStaleTopupOrders(t *testing.T) {
	st := newTestStore(t)
	stale := seedOrder(t, st, model.TopupOrderPending, -20*time.Minute) // 过期 pending
	fresh := seedOrder(t, st, model.TopupOrderPending, -5*time.Minute)  // 未到期
	paid := seedOrder(t, st, model.TopupOrderPaid, -2*time.Hour)        // 早已支付

	if n := ExpireStaleTopupOrders(st, 15*time.Minute); n != 1 {
		t.Fatalf("应恰好迁移 1 笔，实际 %d", n)
	}
	assertStatus := func(id int64, want int, label string) {
		t.Helper()
		var o model.TopupOrder
		if err := st.Read.First(&o, id).Error; err != nil {
			t.Fatalf("查询订单 %s 失败: %v", label, err)
		}
		if o.Status != want {
			t.Fatalf("%s 状态应为 %d，实际 %d", label, want, o.Status)
		}
	}
	assertStatus(stale.ID, model.TopupOrderExpired, "过期 pending 单")
	assertStatus(fresh.ID, model.TopupOrderPending, "未到期 pending 单")
	assertStatus(paid.ID, model.TopupOrderPaid, "已支付单")
	// 幂等：重跑 0 行；timeout<=0 走默认 15min 阈值同样安全。
	if n := ExpireStaleTopupOrders(st, 15*time.Minute); n != 0 {
		t.Fatalf("重跑应迁移 0 笔，实际 %d", n)
	}
	if n := ExpireStaleTopupOrders(st, 0); n != 0 {
		t.Fatalf("默认阈值重跑应迁移 0 笔，实际 %d", n)
	}
}
