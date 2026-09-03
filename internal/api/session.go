// session.go 实现 HMAC-SHA256 签名的会话 cookie（M2-wave1，docs/05）：
// payload（JSON）base64url + "." + HMAC 签名；users.auth_version 纳入签名内容，
// 递增（如改密/封禁）即让全部既有会话失效。TTL 7 天，HttpOnly + SameSite=Lax。
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// SessionCookieName 是会话 cookie 名。
const SessionCookieName = "session"

// sessionTTL 会话有效期。
const sessionTTL = 7 * 24 * time.Hour

// sessionClaims 是会话 payload。AuthV 参与签名校验：与 users.auth_version
// 不一致即视为旧会话（中间件层比对后拒绝）。
type sessionClaims struct {
	UID   int64 `json:"uid"`
	AuthV int64 `json:"authv"`
	Exp   int64 `json:"exp"`
	Iat   int64 `json:"iat"`
}

// SessionManager 签发与校验签名会话 cookie。无共享可变状态，并发安全。
type SessionManager struct {
	secret []byte
	now    func() time.Time // 可注入时钟（测试用）
}

// NewSessionManager 构造。secret 必须非空（main 启动 fail-fast 保证）。
func NewSessionManager(secret []byte) *SessionManager {
	return &SessionManager{secret: secret, now: time.Now}
}

// Issue 为 uid 签发会话并写入 Set-Cookie。
func (m *SessionManager) Issue(c *gin.Context, uid, authv int64) {
	now := m.now()
	payload, _ := json.Marshal(sessionClaims{
		UID:   uid,
		AuthV: authv,
		Exp:   now.Add(sessionTTL).Unix(),
		Iat:   now.Unix(),
	})
	value := base64.RawURLEncoding.EncodeToString(payload) + "." + m.sign(payload)
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// Verify 校验会话 cookie，返回 uid 与签发时的 authv；无效返回 false。
// 顺序：格式 → HMAC（hmac.Equal 恒时比较）→ JSON → 过期。
func (m *SessionManager) Verify(c *gin.Context) (uid, authv int64, ok bool) {
	raw, err := c.Cookie(SessionCookieName)
	if err != nil || raw == "" {
		return 0, 0, false
	}
	payload, sig, found := strings.Cut(raw, ".")
	if !found {
		return 0, 0, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return 0, 0, false
	}
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return 0, 0, false
	}
	if !hmac.Equal(want, m.mac(data)) {
		return 0, 0, false
	}
	var cl sessionClaims
	if err := json.Unmarshal(data, &cl); err != nil {
		return 0, 0, false
	}
	if m.now().Unix() >= cl.Exp {
		return 0, 0, false
	}
	return cl.UID, cl.AuthV, true
}

// Clear 下发过期同名 cookie 清除客户端会话（服务端无状态，不做记账）。
func (m *SessionManager) Clear(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// sign 返回 base64url(HMAC-SHA256(payload))。
func (m *SessionManager) sign(payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(m.mac(payload))
}

// mac 计算 HMAC-SHA256。
func (m *SessionManager) mac(payload []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write(payload)
	return h.Sum(nil)
}
