package gateway

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// newTestAuth 构造注入时钟的鉴权器与临时库。
func newTestAuth(t *testing.T) (*TokenAuth, *store.Store, *atomicInt64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	now := &atomicInt64{v: time.Now().Unix()}
	a := NewTokenAuth(st)
	a.now = func() time.Time { return time.Unix(now.Load(), 0) }
	return a, st, now
}

// atomicInt64 是测试用的原子时钟。
type atomicInt64 struct {
	mu sync.Mutex
	v  int64
}

func (a *atomicInt64) Load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func (a *atomicInt64) Store(v int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

// seedToken 写入测试令牌并返回明文。默认 unlimited（编排级既有测试不依赖账本）；
// 计费链路测试用 mutate 覆盖为带余额令牌（见 billing_test.go）。
func seedToken(t *testing.T, st *store.Store, mutate func(*model.Token)) string {
	t.Helper()
	plain := "sk-test-" + time.Now().Format("150405.000000000")
	tok := &model.Token{
		UserID: 1, Name: "t", Key: plain, KeyHash: HashKey(plain),
		Status: model.StatusEnabled, ExpiredTime: model.EpochForever,
		UnlimitedQuota: true,
	}
	if mutate != nil {
		mutate(tok)
	}
	if err := st.Write.Create(tok).Error; err != nil {
		t.Fatalf("写入令牌失败: %v", err)
	}
	return plain
}

// TestHashKey 已知输入的 SHA-256 hex。
func TestHashKey(t *testing.T) {
	// sha256("abc") = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
	if got := HashKey("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("HashKey 不符: %s", got)
	}
}

// TestAuthenticateLifecycle 有效/无效/禁用/过期与缓存 TTL。
func TestAuthenticateLifecycle(t *testing.T) {
	a, st, now := newTestAuth(t)
	plain := seedToken(t, st, nil)

	// 有效。
	tok, err := a.Authenticate(plain)
	if err != nil || tok == nil || tok.Status != model.StatusEnabled {
		t.Fatalf("有效令牌应通过: %v", err)
	}

	// 无效 key。
	if _, err := a.Authenticate("sk-not-exist"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("无效 key 应 ErrTokenInvalid: %v", err)
	}

	// 缓存生效：库中直接禁用后，TTL 内仍可通过（缓存语义）。
	if err := st.Write.Model(&model.Token{}).Where("key_hash = ?", HashKey(plain)).
		Update("status", model.StatusDisabled).Error; err != nil {
		t.Fatalf("禁用令牌失败: %v", err)
	}
	if _, err := a.Authenticate(plain); err != nil {
		t.Fatalf("TTL 内应命中缓存（仍通过）: %v", err)
	}

	// 推进时钟超过 TTL：缓存失效，重新查库 → 禁用。
	now.Store(now.Load() + int64(AuthCacheTTL/time.Second) + 1)
	if _, err := a.Authenticate(plain); !errors.Is(err, ErrTokenDisabled) {
		t.Fatalf("TTL 过后应感知禁用: %v", err)
	}

	// 过期语义。
	plain2 := seedToken(t, st, func(tk *model.Token) { tk.ExpiredTime = now.Load() - 10 })
	if _, err := a.Authenticate(plain2); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("过期令牌应 ErrTokenExpired: %v", err)
	}
}

// TestInvalidate 写时失效：失效后立即感知库中新状态。
func TestInvalidate(t *testing.T) {
	a, st, _ := newTestAuth(t)
	plain := seedToken(t, st, nil)
	if _, err := a.Authenticate(plain); err != nil {
		t.Fatalf("首查应通过: %v", err)
	}
	// 库中禁用 + Invalidate → 立即拒绝。
	_ = st.Write.Model(&model.Token{}).Where("key_hash = ?", HashKey(plain)).
		Update("status", model.StatusDisabled).Error
	a.Invalidate(HashKey(plain))
	if _, err := a.Authenticate(plain); !errors.Is(err, ErrTokenDisabled) {
		t.Fatalf("Invalidate 后应立即感知禁用: %v", err)
	}
}

// TestSingleflight 并发未命中只触发一次库查询（计数器验证防击穿）。
func TestSingleflight(t *testing.T) {
	a, st, _ := newTestAuth(t)
	plain := seedToken(t, st, nil)

	// 用 gorm 钩子太重：改用包装查询计数不可行——直接验证结果一致性 + 手动合并语义。
	var wg sync.WaitGroup
	var pass atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Authenticate(plain); err == nil {
				pass.Add(1)
			}
		}()
	}
	wg.Wait()
	if pass.Load() != 32 {
		t.Fatalf("并发鉴权应全部通过，实际 %d/32", pass.Load())
	}

	// sfGroup 直接单测：同 key 合并执行次数。
	g := &sfGroup{}
	var calls atomic.Int64
	var wg2 sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			_, _ = g.Do("k", func() (any, error) {
				calls.Add(1)
				time.Sleep(20 * time.Millisecond) // 放大并发窗口
				return 1, nil
			})
		}()
	}
	wg2.Wait()
	if calls.Load() != 1 {
		t.Fatalf("singleflight 应合并为一次执行，实际 %d 次", calls.Load())
	}
}
