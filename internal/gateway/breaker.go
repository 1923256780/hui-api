package gateway

import (
	"sync"
	"time"
)

// 熔断默认参数（docs/10 交接：allowed_fails=3 / cooldown=5s）。
const (
	DefaultAllowedFails   = 3
	DefaultCooldown       = 5 * time.Second
	DefaultMinFailSamples = 10 // 分钟失败率熔断的最小样本量（样本不足不启用失败率判断）
	// FailRateThreshold 分钟失败率阈值：>50% 触发冷却。
	FailRateThreshold = 0.5
	// windowSpan 失败率统计窗口（1 分钟滑动窗口，按样本时间过滤）。
	windowSpan = time.Minute
	// maxSamples 每渠道保留的窗口样本上限（防极端流量撑爆内存）。
	maxSamples = 1024
)

// Breaker 是单个 deployment（渠道）的熔断状态机：
//
//	closed（正常）→ 连续失败达 allowedFails / 429 / 分钟失败率超阈值 → open（冷却）
//	open → CooldownUntil 到期 → 放行探测（半开）→ 成功回 closed / 失败重新 open。
//
// 只隔离单个 deployment：状态按渠道 ID 独立，互不影响。
type Breaker struct {
	mu            sync.Mutex
	fails         int       // 连续失败计数（成功清零）
	cooldownUntil time.Time // 冷却截止时间；零值/过去表示未熔断
	samples       []sample  // 滑动窗口样本（失败率统计）
}

type sample struct {
	at   time.Time
	fail bool
}

// Allow 报告此刻是否放行该 deployment。
// 冷却刚到期时放行探测请求（半开）：探测成功由 OnSuccess 回到 closed，
// 探测失败由 OnFailure 立即重新 open。
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.After(b.cooldownUntil) || now.Equal(b.cooldownUntil)
}

// OnSuccess 记录一次成功：清零连续失败计数。
func (b *Breaker) OnSuccess(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	b.recordLocked(now, false)
}

// OnFailure 记录一次失败，并按规则判定是否进入冷却：
//   - RateLimit（429）：立即冷却（不等计数）；
//   - 连续失败达到 allowedFails：冷却；
//   - 分钟失败率 > 50%（样本量 ≥ MinFailSamples）：冷却。
func (b *Breaker) OnFailure(now time.Time, class ErrClass, cfg BreakerConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordLocked(now, true)
	b.fails++
	trip := class == ClassRateLimit || b.fails >= cfg.AllowedFails
	if !trip {
		if failRate, n := b.windowFailRateLocked(now.Add(-windowSpan)); n >= cfg.MinFailSamples {
			trip = failRate > FailRateThreshold
		}
	}
	if trip {
		b.cooldownUntil = now.Add(cfg.Cooldown)
		b.fails = 0 // 重新计数；冷却期内 Allow 已拒绝
	}
}

// recordLocked 追加窗口样本并淘汰过期样本。
func (b *Breaker) recordLocked(now time.Time, fail bool) {
	b.samples = append(b.samples, sample{at: now, fail: fail})
	if len(b.samples) > maxSamples {
		// 保底裁剪：保留最近一半（正常流量下 1 分钟样本远小于该上限）。
		b.samples = b.samples[len(b.samples)/2:]
	}
	cut := now.Add(-windowSpan)
	idx := 0
	for idx < len(b.samples) && b.samples[idx].at.Before(cut) {
		idx++
	}
	if idx > 0 {
		b.samples = append([]sample(nil), b.samples[idx:]...)
	}
}

// windowFailRateLocked 返回窗口内失败率与样本数（调用方持锁）。
func (b *Breaker) windowFailRateLocked(cut time.Time) (float64, int) {
	total, fails := 0, 0
	for _, s := range b.samples {
		if !s.at.Before(cut) {
			total++
			if s.fail {
				fails++
			}
		}
	}
	if total == 0 {
		return 0, 0
	}
	return float64(fails) / float64(total), total
}

// BreakerConfig 是熔断参数。
type BreakerConfig struct {
	AllowedFails   int
	Cooldown       time.Duration
	MinFailSamples int
}

// DefaultBreakerConfig 返回默认熔断参数。
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		AllowedFails:   DefaultAllowedFails,
		Cooldown:       DefaultCooldown,
		MinFailSamples: DefaultMinFailSamples,
	}
}

// BreakerRegistry 管理全部渠道的熔断状态（并发安全，按需惰性创建）。
type BreakerRegistry struct {
	mu  sync.Mutex
	m   map[int64]*Breaker
	cfg BreakerConfig
	now func() time.Time
}

// NewBreakerRegistry 构造注册表；now 可注入（测试用）。
func NewBreakerRegistry(cfg BreakerConfig, now func() time.Time) *BreakerRegistry {
	if now == nil {
		now = time.Now
	}
	return &BreakerRegistry{m: make(map[int64]*Breaker), cfg: cfg, now: now}
}

// Allow 报告渠道此刻是否可用（未知渠道视为可用）。
func (r *BreakerRegistry) Allow(channelID int64) bool {
	b := r.get(channelID)
	if b == nil {
		return true
	}
	return b.Allow(r.now())
}

// OnSuccess 记录渠道成功。
func (r *BreakerRegistry) OnSuccess(channelID int64) {
	if b := r.get(channelID); b != nil {
		b.OnSuccess(r.now())
	}
}

// OnFailure 记录渠道失败并判定熔断。
func (r *BreakerRegistry) OnFailure(channelID int64, class ErrClass) {
	r.get(channelID).OnFailure(r.now(), class, r.cfg)
}

// CooldownUntil 返回渠道冷却截止时间（未熔断返回零值）——诊断与测试用。
func (r *BreakerRegistry) CooldownUntil(channelID int64) time.Time {
	b := r.get(channelID)
	if b == nil {
		return time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cooldownUntil
}

// Reset 清除渠道熔断状态（管理面「复位」按钮预埋，M3 挂接）。
func (r *BreakerRegistry) Reset(channelID int64) {
	b := r.get(channelID)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	b.cooldownUntil = time.Time{}
	b.samples = nil
}

// get 取（或创建）渠道状态机。
func (r *BreakerRegistry) get(channelID int64) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.m[channelID]
	if !ok {
		b = &Breaker{}
		r.m[channelID] = b
	}
	return b
}
