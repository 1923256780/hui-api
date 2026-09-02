package gateway

import (
	"testing"
	"time"
)

func testNow(base int64) *timeCounter {
	return &timeCounter{cur: time.Unix(base, 0)}
}

// timeCounter 是可推进的测试时钟。
type timeCounter struct{ cur time.Time }

func (c *timeCounter) Now() time.Time { return c.cur }

func (c *timeCounter) Advance(d time.Duration) { c.cur = c.cur.Add(d) }

// TestBreakerAllowedFailsTrip 连续失败达阈值 → 冷却 → 到期恢复。
func TestBreakerAllowedFailsTrip(t *testing.T) {
	clock := testNow(1000)
	b := &Breaker{}
	cfg := DefaultBreakerConfig() // allowed_fails=3, cooldown=5s

	// 前两次失败不熔断。
	b.OnFailure(clock.Now(), ClassServer, cfg)
	b.OnFailure(clock.Now(), ClassServer, cfg)
	if !b.Allow(clock.Now()) {
		t.Fatal("2 次失败不应熔断")
	}
	// 第 3 次失败触发冷却。
	b.OnFailure(clock.Now(), ClassServer, cfg)
	if b.Allow(clock.Now()) {
		t.Fatal("3 次失败应熔断")
	}
	// 冷却期内不可用。
	clock.Advance(4 * time.Second)
	if b.Allow(clock.Now()) {
		t.Fatal("冷却期内不应放行")
	}
	// 到期放行（半开探测）。
	clock.Advance(1 * time.Second)
	if !b.Allow(clock.Now()) {
		t.Fatal("冷却到期应放行探测")
	}
	// 探测成功 → closed。
	b.OnSuccess(clock.Now())
	if !b.Allow(clock.Now()) {
		t.Fatal("探测成功后应恢复")
	}
	// 恢复后失败计数已清零：单次失败不再熔断。
	b.OnFailure(clock.Now(), ClassServer, cfg)
	if !b.Allow(clock.Now()) {
		t.Fatal("计数清零后单次失败不应熔断")
	}
}

// TestBreakerRateLimitImmediateTrip 429 立即冷却（不等计数）。
func TestBreakerRateLimitImmediateTrip(t *testing.T) {
	clock := testNow(2000)
	b := &Breaker{}
	cfg := DefaultBreakerConfig()

	b.OnFailure(clock.Now(), ClassRateLimit, cfg)
	if b.Allow(clock.Now()) {
		t.Fatal("429 应立即冷却")
	}
	// 冷却窗口时长：经 registry 的 CooldownUntil 验证。
	r := NewBreakerRegistry(cfg, clock.Now)
	r.OnFailure(7, ClassRateLimit)
	if got := r.CooldownUntil(7); !got.Equal(clock.Now().Add(cfg.Cooldown)) {
		t.Fatalf("冷却窗口应为 %s，实际截止 %v", cfg.Cooldown, got)
	}
}

// TestBreakerFailRateTrip 分钟失败率超阈值冷却；样本不足不触发。
func TestBreakerFailRateTrip(t *testing.T) {
	clock := testNow(3000)
	b := &Breaker{}
	cfg := BreakerConfig{AllowedFails: 100, Cooldown: 5 * time.Second, MinFailSamples: 10}

	// allowedFails=100 保证不走连续计数路径，只考察失败率路径。
	// 先积累 10 个样本（5 失败 5 成功）：失败率恰为 50% 不触发。
	for i := 0; i < 5; i++ {
		b.OnFailure(clock.Now(), ClassServer, cfg)
	}
	for i := 0; i < 5; i++ {
		b.OnSuccess(clock.Now())
	}
	if !b.Allow(clock.Now()) {
		t.Fatal("失败率恰为 50% 不应触发冷却")
	}
	// 第 11 个失败样本使窗口失败率 6/11 > 50% → 冷却。
	b.OnFailure(clock.Now(), ClassServer, cfg)
	if b.Allow(clock.Now()) {
		t.Fatal("失败率 >50% 应触发冷却")
	}

	// 样本不足（<MinFailSamples）：即使失败率 100% 也不触发失败率路径。
	clock2 := testNow(4000)
	b2 := &Breaker{}
	for i := 0; i < 5; i++ { // 5 < 10
		b2.OnFailure(clock2.Now(), ClassServer, cfg)
	}
	if !b2.Allow(clock2.Now()) {
		t.Fatal("样本不足时不应按失败率熔断")
	}
}

// TestBreakerHalfOpenFailure 冷却到期后探测失败 → 重新冷却。
func TestBreakerHalfOpenFailure(t *testing.T) {
	clock := testNow(5000)
	b := &Breaker{}
	cfg := DefaultBreakerConfig()

	b.OnFailure(clock.Now(), ClassRateLimit, cfg) // 立即冷却
	clock.Advance(cfg.Cooldown)
	if !b.Allow(clock.Now()) {
		t.Fatal("到期应放行探测")
	}
	// 探测失败：trip 时连续计数已清零，需重新累计达阈值才再次冷却。
	b.OnFailure(clock.Now(), ClassServer, cfg)
	b.OnFailure(clock.Now(), ClassServer, cfg)
	if !b.Allow(clock.Now()) {
		t.Fatal("未达阈值不应重新冷却")
	}
	b.OnFailure(clock.Now(), ClassServer, cfg) // 连续第 3 次失败
	if b.Allow(clock.Now()) {
		t.Fatal("探测失败累计达阈值应重新冷却")
	}
}

// TestBreakerIsolation 单渠道熔断不影响其他渠道（只隔离单 deployment）。
func TestBreakerIsolation(t *testing.T) {
	clock := testNow(6000)
	r := NewBreakerRegistry(DefaultBreakerConfig(), clock.Now)

	r.OnFailure(1, ClassServer)
	r.OnFailure(1, ClassServer)
	r.OnFailure(1, ClassServer)
	if r.Allow(1) {
		t.Fatal("渠道 1 应熔断")
	}
	if !r.Allow(2) {
		t.Fatal("渠道 2 不应受渠道 1 影响")
	}
}

// TestBreakerReset 复位清除熔断现场。
func TestBreakerReset(t *testing.T) {
	clock := testNow(7000)
	r := NewBreakerRegistry(DefaultBreakerConfig(), clock.Now)
	r.OnFailure(9, ClassRateLimit)
	if r.Allow(9) {
		t.Fatal("渠道 9 应熔断")
	}
	r.Reset(9)
	if !r.Allow(9) {
		t.Fatal("Reset 后应恢复")
	}
}
