// store_test.go 验证码存储行为测试（M3-wave1，docs/05）：
// TTL 过期、60s 重发限频、每日上限与跨日重置、一次性消费、purpose 维度隔离、
// Sweep 清扫。全部经注入时钟驱动，不依赖真实时间间隔。
package verification

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock 可推进的线程安全时钟（Store.now 注入）。
type fakeClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{cur: time.Date(2026, 9, 3, 12, 0, 0, 0, time.Local)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
}

func newTestStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	return New(clk.Now), clk
}

// TestIssueVerifyOnce 正常路径：签发 → 校验通过 → 重放同码失效（一次性消费）。
func TestIssueVerifyOnce(t *testing.T) {
	s, _ := newTestStore(t)
	code, err := s.Issue("user@example.com", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("验证码应为 6 位，实际 %q", code)
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			t.Fatalf("验证码应为纯数字: %q", code)
		}
	}
	if err := s.Verify("user@example.com", "register", code); err != nil {
		t.Fatalf("正确码应通过: %v", err)
	}
	if err := s.Verify("user@example.com", "register", code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("一次性消费后重放应 ErrNotFound，实际 %v", err)
	}
}

// TestVerifyMismatchNotConsumed 错误码不消耗：修正后原码仍可校验通过。
func TestVerifyMismatchNotConsumed(t *testing.T) {
	s, _ := newTestStore(t)
	code, err := s.Issue("user@example.com", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if err := s.Verify("user@example.com", "register", "000000"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("错误码应 ErrMismatch，实际 %v", err)
	}
	if err := s.Verify("user@example.com", "register", code); err != nil {
		t.Fatalf("错误尝试后正确码仍应通过: %v", err)
	}
}

// TestExpiry TTL 过期：超过 10 分钟后 ErrNotFound（Verify 与 Issue 均惰性忽略）。
func TestExpiry(t *testing.T) {
	s, clk := newTestStore(t)
	code, err := s.Issue("user@example.com", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	clk.Advance(CodeTTL - time.Second)
	if err := s.Verify("user@example.com", "register", code); err != nil {
		t.Fatalf("TTL 内应有效: %v", err)
	}
	clk.Advance(2 * time.Second)
	if err := s.Verify("user@example.com", "register", code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期后应 ErrNotFound，实际 %v", err)
	}
}

// TestResendLimit 60 秒重发限频：间隔内 ErrTooFrequent，间隔后可重发（新码覆盖）。
func TestResendLimit(t *testing.T) {
	s, clk := newTestStore(t)
	first, err := s.Issue("user@example.com", "register")
	if err != nil {
		t.Fatalf("首次签发失败: %v", err)
	}
	clk.Advance(ResendPeriod - time.Second)
	if _, err := s.Issue("user@example.com", "register"); !errors.Is(err, ErrTooFrequent) {
		t.Fatalf("间隔内应 ErrTooFrequent，实际 %v", err)
	}
	clk.Advance(2 * time.Second)
	second, err := s.Issue("user@example.com", "register")
	if err != nil {
		t.Fatalf("间隔后重发应成功: %v", err)
	}
	if second == first {
		t.Fatal("重发应产生新码")
	}
	if err := s.Verify("user@example.com", "register", first); err == nil {
		t.Fatal("旧码应被新码覆盖失效")
	}
	if err := s.Verify("user@example.com", "register", second); err != nil {
		t.Fatalf("新码应有效: %v", err)
	}
}

// TestDailyLimit 每日 20 次上限与跨日重置。
func TestDailyLimit(t *testing.T) {
	s, clk := newTestStore(t)
	email := "daily@example.com"
	for i := 0; i < DailyLimit; i++ {
		clk.Advance(ResendPeriod + time.Second)
		if _, err := s.Issue(email, "register"); err != nil {
			t.Fatalf("第 %d 次签发不应失败: %v", i+1, err)
		}
	}
	clk.Advance(ResendPeriod + time.Second)
	if _, err := s.Issue(email, "register"); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("超过每日上限应 ErrDailyLimit，实际 %v", err)
	}
	// 跨自然日：计数重置。
	clk.Advance(24 * time.Hour)
	if _, err := s.Issue(email, "register"); err != nil {
		t.Fatalf("跨日后应可重发: %v", err)
	}
}

// TestPurposeIsolation purpose 维度隔离：register 与 reset 互不干扰。
func TestPurposeIsolation(t *testing.T) {
	s, _ := newTestStore(t)
	reg, err := s.Issue("user@example.com", "register")
	if err != nil {
		t.Fatalf("register 签发失败: %v", err)
	}
	// reset 与 register 同邮箱同分钟：purpose 维度隔离，不受 register 限频影响。
	reset, err := s.Issue("user@example.com", "reset")
	if err != nil {
		t.Fatalf("reset 签发应不受 register 限频影响: %v", err)
	}
	if err := s.Verify("user@example.com", "reset", reg); !errors.Is(err, ErrMismatch) {
		t.Fatalf("register 码不能过 reset 校验，实际 %v", err)
	}
	if err := s.Verify("user@example.com", "reset", reset); err != nil {
		t.Fatalf("reset 码应通过 reset 校验: %v", err)
	}
}

// TestEmailNormalization 邮箱归一化：大小写与首尾空白不影响同 key 判定。
func TestEmailNormalization(t *testing.T) {
	s, _ := newTestStore(t)
	code, err := s.Issue("  User@Example.COM ", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if err := s.Verify("user@example.com", "register", "      "+code); err != nil {
		t.Fatalf("归一化后应命中同一条目: %v", err)
	}
}

// TestSweep 惰性过期与清扫：过期条目被清除，未过期保留。
func TestSweep(t *testing.T) {
	s, clk := newTestStore(t)
	_, err := s.Issue("old@example.com", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	_, err = s.Issue("fresh@example.com", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if got := s.Len(); got != 2 {
		t.Fatalf("应有 2 条目，实际 %d", got)
	}
	clk.Advance(CodeTTL + time.Second)
	// Verify 亦惰性忽略过期值（不依赖 Sweep 先行）。
	if err := s.Verify("old@example.com", "register", "123456"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期条目 Verify 应 ErrNotFound，实际 %v", err)
	}
	if got := s.Sweep(); got != 2 {
		t.Fatalf("应清扫 2 条过期条目，实际 %d", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("清扫后应为 0 条目，实际 %d", got)
	}
}

// TestStartSweeper 定时清扫启停：停止函数幂等，停止后 goroutine 退出。
func TestStartSweeper(t *testing.T) {
	s, clk := newTestStore(t)
	_, err := s.Issue("sweep@example.com", "register")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	clk.Advance(CodeTTL + time.Second)
	stop := s.StartSweeper(5 * time.Millisecond)
	// 轮询等待清扫生效（至多 1s）。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && s.Len() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Len() != 0 {
		t.Fatal("定时清扫未清除过期条目")
	}
	stop()
	stop() // 幂等
}
