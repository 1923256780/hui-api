// Package verification 实现邮箱验证码的签发与校验（M3-wave1，docs/05）：
// 注册验证码与找回密码验证码共用一套存储（email+purpose 维度隔离）。
//
// 行为约定：
//   - 有效期 10 分钟；同 key（email+purpose）60 秒内不可重复签发；
//   - 同 key 每自然日（服务器本地时区）最多签发 20 次；
//   - 校验成功即一次性消费（删除条目），失败不消耗（可重试，受 TTL 约束）；
//   - Sweep() 清扫已过期条目（Verify/Issue 读取时亦惰性忽略过期值）；
//     StartSweeper 提供定时清扫接口（后台 goroutine）。
//
// 存储：进程内存 map + 互斥锁（验证码是短生命周期临时态，重启丢失可接受，
// 不落库；多副本部署需粘性会话或后续波次改共享存储——docs/10 风险注记）。
package verification

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// 行为参数。
const (
	CodeTTL      = 10 * time.Minute // 验证码有效期
	ResendPeriod = time.Minute      // 同 key 两次签发的最小间隔
	DailyLimit   = 20               // 同 key 每日签发上限
)

// 语义错误（调用方映射：TooFrequent/DailyLimit → 429，NotFound/Mismatch → 400）。
var (
	ErrTooFrequent = errors.New("verification: 发送过于频繁")
	ErrDailyLimit  = errors.New("verification: 超过每日发送上限")
	ErrNotFound    = errors.New("verification: 验证码无效或已过期")
	ErrMismatch    = errors.New("verification: 验证码不正确")
)

// entry 是单个 key 的验证码状态。
type entry struct {
	code      string
	expiresAt time.Time
	resendAt  time.Time
	day       string // 每日计数所属日期（本地时区 yyyy-MM-dd）
	sentCount int    // 当日已签发次数
}

// Store 内存验证码存储：并发安全。
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time // 可注入时钟（测试用）
}

// New 构造存储。now 为 nil 时使用 time.Now。
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{entries: make(map[string]*entry), now: now}
}

// key 归一化维度：purpose|小写 email。
func key(email, purpose string) string {
	return purpose + "|" + strings.ToLower(strings.TrimSpace(email))
}

// Issue 为 email+purpose 签发新验证码。触发 60s 限频或每日上限时返回错误；
// 成功返回 6 位数字码（调用方负责经邮件投递，码不落日志）。
func (s *Store) Issue(email, purpose string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	k := key(email, purpose)
	day := now.Format("2006-01-02")
	if e, ok := s.entries[k]; ok {
		if now.Before(e.resendAt) {
			return "", ErrTooFrequent
		}
		if e.day == day && e.sentCount >= DailyLimit {
			return "", ErrDailyLimit
		}
	}
	code, err := generateCode()
	if err != nil {
		return "", fmt.Errorf("verification: 生成验证码: %w", err)
	}
	count := 1
	if e, ok := s.entries[k]; ok && e.day == day {
		count = e.sentCount + 1
	}
	s.entries[k] = &entry{
		code:      code,
		expiresAt: now.Add(CodeTTL),
		resendAt:  now.Add(ResendPeriod),
		day:       day,
		sentCount: count,
	}
	return code, nil
}

// Verify 校验验证码：不存在/已过期 → ErrNotFound；不匹配 → ErrMismatch；
// 匹配即删除（一次性消费，成功后重放同一码不再通过）。
func (s *Store) Verify(email, purpose, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(email, purpose)
	e, ok := s.entries[k]
	if !ok || s.now().After(e.expiresAt) {
		return ErrNotFound
	}
	if e.code != strings.TrimSpace(code) {
		return ErrMismatch
	}
	delete(s.entries, k)
	return nil
}

// Sweep 清扫已过期条目，返回清除数量（定时器或测试手动调用）。
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	removed := 0
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// StartSweeper 启动后台定时清扫（返回停止函数，调用后资源释放）。
// 生产装配：main 启动时以分钟级 interval 启动，停机时停止。
func (s *Store) StartSweeper(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.Sweep()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// Len 返回当前条目数（诊断与测试用）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// generateCode 生成 6 位数字验证码（crypto/rand；首位可为 0，按字符串比对）。
func generateCode() (string, error) {
	n, err := crand.Int(crand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
