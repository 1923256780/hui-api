// Package ratelimit 实现 hui-api 转发链路的请求限流（M2-wave1，docs/05）：
//
//   - 全局/分组请求限流：滑动窗口内「请求数 + 成功数」双维度限制
//     （options 键 ModelRequestRateLimit*，行业通用命名；分组配置覆盖全局、共用周期）；
//   - 令牌级 TPM/RPM：滑动窗口累计 tokens 消耗与请求次数（tokens.tpm_rpm JSON）。
//
// 记账语义两种（M4 评审修复引入失败维度）：AllowRequest「放行即记账」适用
// 于每次放行都有真实成本的场景（转发、发码）；AllowFailures + TallyFail
// 「失败记账」适用于登录/验码类端点——成功路径零消耗（NAT 共出口 IP 的多
// 用户正常登录不受历史成功影响），连续失败才推进窗口（爆破防护保持）；
// 验码类端点还可在验证通过后经 Reset 清零历史失败（成功即身份成立）。
//
// 存储模型：sync.Map 按 key 分桶，每桶独立互斥锁 + 按时间追加的事件序列；
// 访问时惰性淘汰窗口外事件；条目总数超上限时对空闲桶做 TTL 清扫（LRU 近似），
// 保证极端 key 基数（海量 IP / 令牌）下内存有界。判定与记录在同一次调用内完成，
// 调用方无需二次加锁。
package ratelimit

import (
	"sync"
	"sync/atomic"
	"time"
)

// TokenWindow 是令牌级 TPM/RPM 的固定窗口长度（TPM/RPM 语义即「每分钟」）。
const TokenWindow = time.Minute

// 容量参数。
const (
	// DefaultMaxKeys 同时刻存的窗口条目上限（超过即触发空闲清扫）。
	DefaultMaxKeys = 8192
	// idleTTL 空闲条目存活时长：超过该时长未被访问的条目在清扫时整体删除。
	// 取值需覆盖最长可配窗口（分钟级），避免活跃条目被误删。
	idleTTL = 2 * time.Hour
)

// Limiter 是滑动窗口限流器：并发安全，零值不可用，经 New 构造。
type Limiter struct {
	now      func() time.Time
	windows  sync.Map // key string → *bucket
	count    atomic.Int64
	sweeping atomic.Bool
	maxKeys  int64
}

// New 构造限流器。now 允许注入（测试用）；nil 表示 time.Now。
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, maxKeys: DefaultMaxKeys}
}

// bucket 是单个 key 的多维度事件序列（请求数/成功数/tokens 用量共享一个条目）。
type bucket struct {
	mu         sync.Mutex
	lastAccess time.Time
	reqs       []time.Time // 请求时间戳（请求数维度）
	succs      []time.Time // 成功完成时间戳（成功数维度）
	tokens     []tokenUse  // tokens 消耗事件（TPM 维度）
	fails      []time.Time // 失败记账时间戳（AllowFailures/TallyFail 维度）
}

type tokenUse struct {
	at time.Time
	n  int
}

// AllowRequest 判定请求是否放行（放行即记录一次请求时间戳）。
// maxRequests<=0 表示不限请求数；maxSuccess<=0 表示不限成功数。
// 拒绝时返回建议的 Retry-After 时长（恒 >0）：两个约束中更晚解除者。
func (l *Limiter) AllowRequest(key string, window time.Duration, maxRequests, maxSuccess int) (bool, time.Duration) {
	b := l.bucketFor(key)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = now
	b.pruneLocked(now, window)

	var retry time.Duration
	if maxRequests > 0 && len(b.reqs) >= maxRequests {
		retry = untilFree(now, window, b.reqs[0])
	}
	if maxSuccess > 0 && len(b.succs) >= maxSuccess {
		if r := untilFree(now, window, b.succs[0]); r > retry {
			retry = r
		}
	}
	if retry > 0 {
		return false, retry
	}
	b.reqs = append(b.reqs, now)
	return true, 0
}

// RecordSuccess 记录一次成功完成（成功数维度；失败/被拒请求不记）。
func (l *Limiter) RecordSuccess(key string, window time.Duration) {
	b := l.bucketFor(key)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = now
	b.pruneLocked(now, window)
	b.succs = append(b.succs, now)
}

// AllowFailures 以失败次数为预算维度预检（纯判定，不记账）：窗口内失败数
// 未达 max 即放行。与 AllowRequest 的「放行即记账」不同，记账由调用方在
// 确认失败（凭据校验失败/验码不匹配等）后经 TallyFail 完成——成功响应不
// 消耗预算，失败才推进窗口（登录爆破防护语义：连续失败仍受限，正常登录
// 不受历史成功拖累）。max<=0 表示不限；拒绝时返回建议 Retry-After（恒 >0）。
func (l *Limiter) AllowFailures(key string, window time.Duration, max int) (bool, time.Duration) {
	if max <= 0 {
		return true, 0
	}
	b := l.bucketFor(key)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = now
	b.pruneLocked(now, window)
	if len(b.fails) >= max {
		return false, untilFree(now, window, b.fails[0])
	}
	return true, 0
}

// TallyFail 记录一次失败（失败预算维度，消耗一次 AllowFailures 配额）。
func (l *Limiter) TallyFail(key string, window time.Duration) {
	b := l.bucketFor(key)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = now
	b.pruneLocked(now, window)
	b.fails = append(b.fails, now)
}

// Reset 清空 key 的失败记账预算（验证通过后调用）：验码类端点凭「验证通过
// 即身份成立」清零历史失败（如 TOTP enable/disable 正确码通过后），后续
// 尝试从零计数；登录类不 Reset（成功不清账，爆破防护不受正常登录重置）。
// key 从未记账时为幂等 no-op。
func (l *Limiter) Reset(key string) {
	v, ok := l.windows.Load(key)
	if !ok {
		return
	}
	b := v.(*bucket)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = l.now()
	b.fails = nil
}

// AllowTokens 判定令牌级 TPM/RPM 是否放行（放行即记录一次请求时间戳，RPM 维度）。
// tpm<=0 或 rpm<=0 分别表示对应维度不限。拒绝时返回建议 Retry-After（恒 >0）。
//
// TPM 判定的是「当前窗口内已消耗 tokens 是否达到上限」：实际用量由调用方在请求
// 完成后经 RecordTokenUsage 累计，限流在请求间生效、不做请求内截断。
func (l *Limiter) AllowTokens(key string, tpm, rpm int) (bool, time.Duration) {
	b := l.bucketFor(key)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = now
	b.pruneLocked(now, TokenWindow)

	var retry time.Duration
	if rpm > 0 && len(b.reqs) >= rpm {
		retry = untilFree(now, TokenWindow, b.reqs[0])
	}
	if tpm > 0 {
		total := 0
		for _, u := range b.tokens {
			total += u.n
		}
		if total >= tpm {
			// 最早使剩余用量降到 tpm 以下的过期点即建议等待时长。
			removed := 0
			for _, u := range b.tokens {
				removed += u.n
				if total-removed < tpm {
					if r := u.at.Add(TokenWindow).Sub(now); r > retry {
						retry = r
					}
					break
				}
			}
			if retry <= 0 {
				retry = time.Second // 防御兜底：异常时钟下给最小等待
			}
		}
	}
	if retry > 0 {
		return false, retry
	}
	b.reqs = append(b.reqs, now)
	return true, 0
}

// RecordTokenUsage 累计一次请求的实际 tokens 用量（TPM 维度；tokens<=0 忽略）。
func (l *Limiter) RecordTokenUsage(key string, tokens int) {
	if tokens <= 0 {
		return
	}
	b := l.bucketFor(key)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAccess = now
	b.pruneLocked(now, TokenWindow)
	b.tokens = append(b.tokens, tokenUse{at: now, n: tokens})
}

// WindowStats 返回 key 当前窗口内的事件计数（诊断与测试用）。
func (l *Limiter) WindowStats(key string, window time.Duration) (requests, successes, tokens int) {
	v, ok := l.windows.Load(key)
	if !ok {
		return 0, 0, 0
	}
	b := v.(*bucket)
	now := l.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now, window)
	total := 0
	for _, u := range b.tokens {
		total += u.n
	}
	return len(b.reqs), len(b.succs), total
}

// Len 返回当前条目数（诊断与测试用）。
func (l *Limiter) Len() int64 { return l.count.Load() }

// bucketFor 取（或创建）key 的窗口条目；创建后条目超上限时触发空闲清扫。
func (l *Limiter) bucketFor(key string) *bucket {
	if v, ok := l.windows.Load(key); ok {
		return v.(*bucket)
	}
	b := &bucket{}
	v, loaded := l.windows.LoadOrStore(key, b)
	if loaded {
		return v.(*bucket)
	}
	if l.count.Add(1) > l.maxKeys {
		l.sweepIdle()
	}
	return b
}

// sweepIdle 清扫空闲条目（lastAccess 早于 idleTTL 的桶整体删除，LRU 近似）。
// 并发调用合并为一次执行；逐桶短持锁，不与调用方桶锁嵌套。
func (l *Limiter) sweepIdle() {
	if !l.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer l.sweeping.Store(false)
	cut := l.now().Add(-idleTTL)
	l.windows.Range(func(key, v any) bool {
		b := v.(*bucket)
		b.mu.Lock()
		idle := b.lastAccess.Before(cut)
		b.mu.Unlock()
		if idle && l.windows.CompareAndDelete(key, v) {
			l.count.Add(-1)
		}
		return true
	})
}

// pruneLocked 淘汰窗口外事件（调用方持锁）。事件按时间追加、天然有序。
func (b *bucket) pruneLocked(now time.Time, window time.Duration) {
	b.reqs = pruneTimes(b.reqs, now, window)
	b.succs = pruneTimes(b.succs, now, window)
	b.fails = pruneTimes(b.fails, now, window)
	cut := now.Add(-window)
	idx := 0
	for idx < len(b.tokens) && b.tokens[idx].at.Before(cut) {
		idx++
	}
	if idx > 0 {
		b.tokens = append([]tokenUse(nil), b.tokens[idx:]...)
	}
}

// pruneTimes 返回裁剪掉窗口外前缀后的时间戳序列。
func pruneTimes(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cut := now.Add(-window)
	idx := 0
	for idx < len(ts) && ts[idx].Before(cut) {
		idx++
	}
	if idx == 0 {
		return ts
	}
	return append([]time.Time(nil), ts[idx:]...)
}

// untilFree 返回 oldest 事件滑出窗口所需的剩余时长（恒 >0）。
func untilFree(now time.Time, window time.Duration, oldest time.Time) time.Duration {
	r := oldest.Add(window).Sub(now)
	if r <= 0 {
		// 已过期却仍超限：时钟回拨等异常场景的最小等待兜底。
		return time.Second
	}
	return r
}
