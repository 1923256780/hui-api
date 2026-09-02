package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// 鉴权缓存参数：key_hash → 令牌 内存缓存 TTL 30s；管理面写令牌后应调用
// Invalidate/InvalidateAll 写时失效（M3 挂接）。防击穿由 singleflight 保证：
// 同一 key_hash 的并发未命中只查库一次。不存在的 key 做短 TTL 负缓存防穿透。
const (
	AuthCacheTTL      = 30 * time.Second
	AuthNegativeTTL   = 5 * time.Second
	AuthCacheMaxEntry = 4096 // 缓存条目上限（超限整体失效重建，令牌量级远低于此）
)

// 鉴权语义错误（按协议映射 401/403）。
var (
	ErrTokenInvalid  = errors.New("invalid api key")
	ErrTokenDisabled = errors.New("token disabled")
	ErrTokenExpired  = errors.New("token expired")
)

// HashKey 计算令牌明文的 SHA-256 hex（tokens.key_hash 鉴权唯一依据）。
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

type authEntry struct {
	token *model.Token // nil 表示负缓存（key 不存在）
	exp   time.Time
}

// TokenAuth 是令牌鉴权器：哈希查找 + 内存缓存 + singleflight。
type TokenAuth struct {
	st     *store.Store
	ttl    time.Duration
	negTTL time.Duration
	now    func() time.Time

	mu    sync.RWMutex
	cache map[string]authEntry
	sf    sfGroup
}

// NewTokenAuth 构造鉴权器。
func NewTokenAuth(st *store.Store) *TokenAuth {
	return &TokenAuth{
		st:     st,
		ttl:    AuthCacheTTL,
		negTTL: AuthNegativeTTL,
		now:    time.Now,
		cache:  make(map[string]authEntry),
	}
}

// Authenticate 校验明文密钥：SHA-256 → 缓存/库查 tokens.key_hash，
// 校验 status 与有效期。返回的 Token 是快照拷贝，调用方可安全读取。
func (a *TokenAuth) Authenticate(plainKey string) (*model.Token, error) {
	keyHash := HashKey(strings.TrimSpace(plainKey))

	if tok, ok := a.getCached(keyHash); ok {
		if tok == nil {
			return nil, ErrTokenInvalid
		}
		if err := validateToken(tok, a.now().Unix()); err != nil {
			return nil, err
		}
		return tok, nil
	}

	// singleflight：并发未命中只触发一次库查询，其余等待共享结果。
	v, err := a.sf.Do(keyHash, func() (any, error) {
		return a.lookup(keyHash)
	})
	if err != nil {
		return nil, err
	}
	entry := v.(authEntry)
	a.putCached(keyHash, entry)

	if entry.token == nil {
		return nil, ErrTokenInvalid
	}
	if err := validateToken(entry.token, a.now().Unix()); err != nil {
		return nil, err
	}
	return entry.token, nil
}

// lookup 查库并构造缓存条目（含负缓存）。
func (a *TokenAuth) lookup(keyHash string) (any, error) {
	var row model.Token
	err := a.st.Read.Where("key_hash = ?", keyHash).First(&row).Error
	now := a.now()
	if err != nil {
		if errors.Is(err, errRecordNotFound) {
			return authEntry{token: nil, exp: now.Add(a.negTTL)}, nil
		}
		return nil, fmt.Errorf("查询令牌: %w", err)
	}
	tok := row // 拷贝，避免缓存条目被外部修改
	return authEntry{token: &tok, exp: now.Add(a.ttl)}, nil
}

// getCached 读取缓存（过期淘汰）。
func (a *TokenAuth) getCached(keyHash string) (*model.Token, bool) {
	a.mu.RLock()
	e, ok := a.cache[keyHash]
	a.mu.RUnlock()
	if !ok || a.now().After(e.exp) {
		return nil, false
	}
	return e.token, true
}

// putCached 写缓存（超限整体失效重建：简单且有界）。
func (a *TokenAuth) putCached(keyHash string, e authEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cache) >= AuthCacheMaxEntry {
		a.cache = make(map[string]authEntry)
	}
	a.cache[keyHash] = e
}

// Invalidate 写时失效：管理面更新/删除令牌后调用（M3 挂接）。
func (a *TokenAuth) Invalidate(keyHash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cache, keyHash)
}

// InvalidateAll 清空全部缓存（管理面批量操作后调用）。
func (a *TokenAuth) InvalidateAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cache = make(map[string]authEntry)
}

// validateToken 校验状态与有效期（缓存命中路径同样执行，保证禁用/过期即时生效语义
// 与缓存 TTL 一致：最长 30s 内收敛）。
func validateToken(tok *model.Token, nowUnix int64) error {
	if tok.Status != model.StatusEnabled {
		return ErrTokenDisabled
	}
	if tok.ExpiredTime != model.EpochForever && tok.ExpiredTime < nowUnix {
		return ErrTokenExpired
	}
	return nil
}

// errRecordNotFound 引用 gorm 的记录不存在哨兵（store 层统一语义）。
var errRecordNotFound = gorm.ErrRecordNotFound

// sfGroup 是手写 singleflight：同 key 并发调用合并为一次执行。
// 相比引入 x/sync 依赖，这里只需要最小语义（合并 + 共享结果 + 广播错误）。
type sfGroup struct {
	mu    sync.Mutex
	calls map[string]*sfCall
}

type sfCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Do 合并同 key 的并发调用：首个调用执行 fn，其余等待并共享其结果。
func (g *sfGroup) Do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*sfCall)
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return c.val, c.err
}
