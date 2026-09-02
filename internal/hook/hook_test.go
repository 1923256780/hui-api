package hook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// collector 是线程安全的测试 Hook，记录收到的成功/失败事件。
type collector struct {
	mu       sync.Mutex
	name     string
	success  []Event
	failures []Event
}

func newCollector(name string) *collector { return &collector{name: name} }

func (c *collector) Name() string { return c.name }

func (c *collector) OnSuccess(_ context.Context, ev Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.success = append(c.success, ev)
	return nil
}

func (c *collector) OnFailure(_ context.Context, ev Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = append(c.failures, ev)
	return nil
}

func (c *collector) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.success), len(c.failures)
}

// waitUntil 轮询等待条件成立，超时返回错误。
func waitUntil(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("等待条件超时")
}

// TestRegistryDuplicateName 同名注册必须拒绝。
func TestRegistryDuplicateName(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(Noop{}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if err := reg.Register(Noop{}); err == nil {
		t.Fatal("同名重复注册应报错")
	}
	if err := reg.Register(newCollector("other")); err != nil {
		t.Fatalf("注册其他名字失败: %v", err)
	}
	if reg.Count() != 2 {
		t.Fatalf("注册表数量期望 2，实际 %d", reg.Count())
	}
	reg.Unregister("noop")
	reg.Unregister("noop") // 幂等
	if reg.Count() != 1 {
		t.Fatalf("注销后数量期望 1，实际 %d", reg.Count())
	}
}

// TestRegistryListSortedOrder List 按名字排序，投递顺序稳定。
func TestRegistryListSortedOrder(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(newCollector("zeta"))
	_ = reg.Register(newCollector("alpha"))
	_ = reg.Register(newCollector("mid"))
	got := ""
	for _, h := range reg.List() {
		got += h.Name()
	}
	if got != "alphamidzeta" {
		t.Fatalf("List 应按名字排序，实际 %q", got)
	}
}

// TestDispatchDeliversAll 事件全部到达所有 Hook。
func TestDispatchDeliversAll(t *testing.T) {
	reg := NewRegistry()
	c1 := newCollector("one")
	c2 := newCollector("two")
	_ = reg.Register(c1)
	_ = reg.Register(c2)
	_ = reg.Register(NewNoop())

	d := NewDispatcher(reg, 64)
	d.Start(2)
	defer d.Stop(time.Second)

	const total = 50
	for i := 0; i < total; i++ {
		if ok := d.Dispatch(Event{Type: "test", RequestID: fmt.Sprintf("req-%d", i)}); !ok {
			t.Fatalf("队列容量 %d 不应丢弃第 %d 个事件", 64, i)
		}
	}
	if err := waitUntil(2*time.Second, func() bool { return d.Processed() == total }); err != nil {
		t.Fatalf("事件未全部处理: %v（processed=%d）", err, d.Processed())
	}
	if n, _ := c1.counts(); n != total {
		t.Fatalf("hook one 期望 %d 个事件，实际 %d", total, n)
	}
	if n, _ := c2.counts(); n != total {
		t.Fatalf("hook two 期望 %d 个事件，实际 %d", total, n)
	}
	if d.Dropped() != 0 {
		t.Fatalf("不应有丢弃，实际 %d", d.Dropped())
	}
}

// TestFailureRoutedToOnFailure 失败事件（Err 非空）只进 OnFailure。
func TestFailureRoutedToOnFailure(t *testing.T) {
	reg := NewRegistry()
	c := newCollector("router")
	_ = reg.Register(c)

	d := NewDispatcher(reg, 8)
	d.Start(1)
	defer d.Stop(time.Second)

	_ = d.Dispatch(Event{Type: "t", RequestID: "ok-1"})
	_ = d.Dispatch(Event{Type: "t", RequestID: "bad-1", Err: "upstream 500"})

	if err := waitUntil(2*time.Second, func() bool {
		s, f := c.counts()
		return s == 1 && f == 1
	}); err != nil {
		s, f := c.counts()
		t.Fatalf("路由不正确: success=%d failure=%d", s, f)
	}
}

// TestQueueFullDropsAndCounts 队列满时非阻塞丢弃并计数。
func TestQueueFullDropsAndCounts(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(newCollector("sink"))

	const queueSize = 2
	d := NewDispatcher(reg, queueSize)
	// 不 Start：队列只进不出。

	accepted := 0
	for i := 0; i < 10; i++ {
		if d.Dispatch(Event{Type: "t", RequestID: fmt.Sprintf("r-%d", i)}) {
			accepted++
		}
	}
	if accepted != queueSize {
		t.Fatalf("未消费时期望恰好接受 %d 个，实际 %d", queueSize, accepted)
	}
	if d.Dropped() != 10-queueSize {
		t.Fatalf("期望丢弃 %d 个，实际 %d", 10-queueSize, d.Dropped())
	}

	// Stop 后再投递同样计为丢弃。
	d.Stop(time.Second)
	if d.Dispatch(Event{Type: "t"}) {
		t.Fatal("Stop 后 Dispatch 应返回 false")
	}
	if d.Dropped() != 10-queueSize+1 {
		t.Fatalf("Stop 后丢弃计数期望 %d，实际 %d", 10-queueSize+1, d.Dropped())
	}
}

// TestHookPanicDoesNotKillWorker 单 hook panic 不影响后续事件处理。
func TestHookPanicDoesNotKillWorker(t *testing.T) {
	reg := NewRegistry()
	c := newCollector("safe")
	_ = reg.Register(c)
	_ = reg.Register(panicHook{})

	d := NewDispatcher(reg, 8)
	d.Start(1)
	defer d.Stop(time.Second)

	for i := 0; i < 3; i++ {
		_ = d.Dispatch(Event{Type: "t", RequestID: fmt.Sprintf("p-%d", i)})
	}
	if err := waitUntil(2*time.Second, func() bool { return d.Processed() == 3 }); err != nil {
		t.Fatalf("panic 后事件应继续处理: %v（processed=%d）", err, d.Processed())
	}
	if n, _ := c.counts(); n != 3 {
		t.Fatalf("panic hook 不应影响其他 hook，期望 3 实际 %d", n)
	}
}

// panicHook 每次 OnSuccess 都 panic 的测试 Hook。
type panicHook struct{}

func (panicHook) Name() string { return "panic" }

func (panicHook) OnSuccess(context.Context, Event) error { panic("boom") }

func (panicHook) OnFailure(context.Context, Event) error { return nil }
