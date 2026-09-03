// hooks_test.go gateway→hook 观测旁路挂接测试（M2-wave3）：
// 成功请求投递 request.completed（Data 含 quota/tokens/duration_ms），失败请求
// 投递 request.failed（含错误摘要），总开关关闭时不投递。
package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/hook"
)

// captureHook 捕获事件的测试 Hook。
type captureHook struct {
	mu  sync.Mutex
	evs []hook.Event
}

func (c *captureHook) Name() string { return "capture" }

func (c *captureHook) OnSuccess(_ context.Context, ev hook.Event) error {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
	return nil
}

func (c *captureHook) OnFailure(_ context.Context, ev hook.Event) error {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
	return nil
}

func (c *captureHook) snapshot() []hook.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hook.Event(nil), c.evs...)
}

// waitForEvents 轮询等待捕获至少 n 个事件（异步旁路）。
func waitForEvents(t *testing.T, c *captureHook, n int) []hook.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if evs := c.snapshot(); len(evs) >= n {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个事件超时，实际 %d", n, len(c.snapshot()))
	return nil
}

// attachCapture 构造挂接 capture hook 的分发器。
func attachCapture(t *testing.T, g *Gateway) *captureHook {
	t.Helper()
	cap := &captureHook{}
	reg := hook.NewRegistry()
	if err := reg.Register(cap); err != nil {
		t.Fatalf("注册 capture hook 失败: %v", err)
	}
	d := hook.NewDispatcher(reg, 16)
	d.Start(1)
	t.Cleanup(func() { d.Stop(time.Second) })
	g.SetHooks(d)
	return cap
}

// TestGatewayHookCompleted 成功请求投递 completed 事件，数据完整。
func TestGatewayHookCompleted(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)
	cap := attachCapture(t, g)

	w := postChat(t, g, seedQuotaToken(t, st, 100000), []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	evs := waitForEvents(t, cap, 1)
	var found *hook.Event
	for i := range evs {
		if evs[i].Type == "request.completed" {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatalf("应投递 completed 事件，实际 %v", evs)
	}
	if found.Model != "m1" || found.TokenID == 0 || found.RequestID == "" {
		t.Fatalf("事件上下文不符: %+v", found)
	}
	if found.IdempotencyKey == "" {
		t.Fatalf("幂等键不应为空")
	}
	if quota, ok := found.Data["quota"].(int64); !ok || quota <= 0 {
		t.Fatalf("quota 应为正实结值: %v", found.Data["quota"])
	}
	if _, ok := found.Data["duration_ms"]; !ok {
		t.Fatalf("应含 duration_ms: %v", found.Data)
	}
	if found.Data["prompt_tokens"].(int) != 12 || found.Data["completion_tokens"].(int) != 8 {
		t.Fatalf("tokens 口径不符: %v", found.Data)
	}
}

// TestGatewayHookFailed 失败请求投递 failed 事件（无可用渠道路径）。
func TestGatewayHookFailed(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	plain := seedQuotaToken(t, st, 100000)
	cap := attachCapture(t, g)

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("无渠道应 503，实际 %d", w.Code)
	}

	evs := waitForEvents(t, cap, 1)
	var found *hook.Event
	for i := range evs {
		if evs[i].Type == "request.failed" {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatalf("应投递 failed 事件，实际 %v", evs)
	}
	if found.Err == "" {
		t.Fatalf("失败事件应含错误摘要: %+v", found)
	}
	if found.Model != "m1" {
		t.Fatalf("事件模型不符: %+v", found)
	}
}

// TestGatewayHookDisabled 总开关关闭：不投递任何事件。
func TestGatewayHookDisabled(t *testing.T) {
	g, st, _ := newTestGateway(t, map[string]string{OptionKeyHooksEnabled: "false"})
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)
	cap := attachCapture(t, g)

	w := postChat(t, g, seedQuotaToken(t, st, 100000), []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if evs := cap.snapshot(); len(evs) != 0 {
		t.Fatalf("开关关闭不应投递，实际 %v", evs)
	}
}
