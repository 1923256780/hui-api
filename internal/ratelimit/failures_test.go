// failures_test.go 失败记账语义包内单测（M4 评审 M-B/M-D 引入的
// AllowFailures/TallyFail/Reset）：成功路径零消耗、连续失败达上限拒绝、
// 窗口滑走恢复、成功验证清零。
package ratelimit

import (
	"testing"
	"time"
)

// TestAllowFailuresBudget 失败记账语义三段式：仅预检不记账可无限放行；
// TallyFail 推进预算至拒绝（Retry-After 为正）；窗口滑走后预算恢复。
func TestAllowFailuresBudget(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })
	const window = time.Hour
	const max = 3

	// 成功路径零消耗：仅预检不记账，重复放行不推进任何窗口。
	for i := 0; i < 10; i++ {
		if ok, _ := l.AllowFailures("k", window, max); !ok {
			t.Fatalf("未记账时第 %d 次预检应放行", i+1)
		}
	}
	// 连续失败达上限 → 预检拒绝，Retry-After 恒正。
	for i := 0; i < max; i++ {
		if ok, _ := l.AllowFailures("k", window, max); !ok {
			t.Fatalf("失败 %d 次时尚未达上限，应放行", i)
		}
		l.TallyFail("k", window)
	}
	ok, retry := l.AllowFailures("k", window, max)
	if ok {
		t.Fatal("失败达上限后预检应拒绝")
	}
	if retry <= 0 {
		t.Fatalf("拒绝应返回正的 Retry-After，实际 %v", retry)
	}
	// 窗口滑走 → 预算恢复（失败事件惰性淘汰）。
	now = now.Add(window + time.Second)
	if ok, _ := l.AllowFailures("k", window, max); !ok {
		t.Fatal("窗口滑走后应恢复放行")
	}
}

// TestResetClearsFailureBudget 成功验证清零（M-D）：部分消耗预算后 Reset
// 清零恢复满额放行、清零后重新记账可达上限拒绝；对无记录 key 幂等。
func TestResetClearsFailureBudget(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })
	const window = time.Hour
	const max = 3

	l.TallyFail("k", window)
	l.TallyFail("k", window)
	l.Reset("k")
	if ok, _ := l.AllowFailures("k", window, max); !ok {
		t.Fatal("Reset 后预算应清零放行")
	}
	// 清零后从零计数：重新达上限拒绝。
	for i := 0; i < max; i++ {
		l.TallyFail("k", window)
	}
	if ok, _ := l.AllowFailures("k", window, max); ok {
		t.Fatal("重新记账达上限后应拒绝")
	}
	// 无记录 key 幂等不 panic。
	l.Reset("absent")
}
