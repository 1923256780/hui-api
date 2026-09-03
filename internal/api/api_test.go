package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// newTestAPI 构造临时库 + 可推进时钟的会话管理器 + 已注册 /api 路由的引擎，
// 并注册两个鉴权探测路由（_probe_auth / _probe_root）。返回引擎、库、处理器。
func newTestAPI(t *testing.T) (*gin.Engine, *store.Store, *Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	rt, err := config.NewRuntime(st)
	if err != nil {
		t.Fatalf("构造运行轨失败: %v", err)
	}
	pricer, err := billing.NewEngine(rt)
	if err != nil {
		t.Fatalf("构造计费引擎失败: %v", err)
	}
	gw := gateway.New(st, rt, pricer)
	t.Cleanup(gw.Close)
	gin.SetMode(gin.TestMode)
	now := time.Unix(0, 0)
	sess := NewSessionManager([]byte("test-secret"))
	sess.now = func() time.Time { return now }

	h := New(st, rt, gw, sess)
	r := gin.New()
	h.Register(r)
	r.GET("/api/_probe_auth", h.RequireAuth, func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/api/_probe_root", h.RequireRoot, func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r, st, h
}

// doJSON 发起一次 JSON 请求；cookie 为原始 Cookie 头值（可为空），body 序列化为 JSON。
func doJSON(t *testing.T, r *gin.Engine, method, path, cookie string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// sessionCookieFrom 从登录/签发响应的 Set-Cookie 头提取「name=value」对。
func sessionCookieFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	sc := w.Header().Get("Set-Cookie")
	if sc == "" {
		t.Fatal("响应应携带 Set-Cookie")
	}
	pair := strings.Split(sc, ";")[0]
	if !strings.HasPrefix(pair, SessionCookieName+"=") || strings.TrimPrefix(pair, SessionCookieName+"=") == "" {
		t.Fatalf("Set-Cookie 应为非空 session 值: %q", sc)
	}
	return pair
}

// seedUser 写入一个指定角色的用户并返回（口令经 bcrypt 哈希）。
func seedUser(t *testing.T, st *store.Store, username, password string, role int) model.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("哈希口令失败: %v", err)
	}
	u := model.User{
		Username: username, PasswordHash: hash, Role: role,
		Status: model.StatusEnabled, AuthVersion: 1, CreatedTime: time.Now().Unix(),
	}
	if err := st.Write.Create(&u).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	return u
}

// TestPasswordHashRoundTrip bcrypt 哈希可校验通过、错口令拒绝、同口令两次哈希不同。
func TestPasswordHashRoundTrip(t *testing.T) {
	h1, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	if !CheckPassword(h1, "s3cret!") {
		t.Fatal("正确口令应校验通过")
	}
	if CheckPassword(h1, "wrong") {
		t.Fatal("错误口令应拒绝")
	}
	h2, _ := HashPassword("s3cret!")
	if h1 == h2 {
		t.Fatal("bcrypt 加盐：同口令两次哈希应不同")
	}
}

// TestSessionRoundTrip 会话签发后可校验，uid/authv 一致；无 cookie 无效。
func TestSessionRoundTrip(t *testing.T) {
	sess := NewSessionManager([]byte("test-secret"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	sess.Issue(c, 42, 3)
	pair := sessionCookieFrom(t, w)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", pair)
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request = req
	uid, authv, ok := sess.Verify(c2)
	if !ok || uid != 42 || authv != 3 {
		t.Fatalf("会话应有效且 uid/authv 一致，实际 ok=%v uid=%d authv=%d", ok, uid, authv)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	c3, _ := gin.CreateTestContext(httptest.NewRecorder())
	c3.Request = req3
	if _, _, ok := sess.Verify(c3); ok {
		t.Fatal("无 cookie 应无效")
	}
}

// TestSessionTamperRejected 篡改 payload 或签名任意部分均拒绝。
// 用确定不同的字符替换：固定 'A'/'B' 在原字符相同时（约 1/64 概率，随时间戳
// 内容漂移偶发）篡改实际未变，会误报失败。
func TestSessionTamperRejected(t *testing.T) {
	sess := NewSessionManager([]byte("test-secret"))
	flipChar := func(s string, i int) string {
		next := byte('A')
		if s[i] == 'A' {
			next = 'B'
		}
		return s[:i] + string(next) + s[i+1:]
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	sess.Issue(c, 1, 1)
	raw := strings.Split(w.Header().Get("Set-Cookie"), ";")[0]
	value := strings.TrimPrefix(raw, SessionCookieName+"=")

	for name, mutated := range map[string]string{
		"payload": flipChar(value, len(value)/2), // 翻转中部一个字符
		"sign":    flipChar(value, len(value)-1),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Cookie", SessionCookieName+"="+mutated)
		c2, _ := gin.CreateTestContext(httptest.NewRecorder())
		c2.Request = req
		if _, _, ok := sess.Verify(c2); ok {
			t.Fatalf("篡改 %s 后应无效", name)
		}
	}
}

// TestSessionExpiredRejected 注入时钟推进超过 TTL 后会话过期。
func TestSessionExpiredRejected(t *testing.T) {
	now := time.Unix(0, 0)
	sess := NewSessionManager([]byte("test-secret"))
	sess.now = func() time.Time { return now }

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	sess.Issue(c, 1, 1)
	pair := strings.Split(w.Header().Get("Set-Cookie"), ";")[0]

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", pair)
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request = req
	if _, _, ok := sess.Verify(c2); !ok {
		t.Fatal("TTL 内会话应有效")
	}

	now = now.Add(sessionTTL + time.Minute)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Cookie", pair)
	c3, _ := gin.CreateTestContext(httptest.NewRecorder())
	c3.Request = req2
	if _, _, ok := sess.Verify(c3); ok {
		t.Fatal("超过 TTL 后会话应过期")
	}
}

// TestEnsureRootUserIdempotent 首次创建 root（口令来自 env）、二次幂等跳过。
func TestEnsureRootUserIdempotent(t *testing.T) {
	_, st, _ := newTestAPI(t)
	t.Setenv("HUI_API_ROOT_PASSWORD", "env-root-pass")

	created, err := EnsureRootUser(st)
	if err != nil || !created {
		t.Fatalf("首次应创建 root，created=%v err=%v", created, err)
	}
	var u model.User
	if err := st.Read.Where("username = ?", RootUsername).First(&u).Error; err != nil {
		t.Fatalf("root 用户应存在: %v", err)
	}
	if u.Role != model.RoleAdmin || !CheckPassword(u.PasswordHash, "env-root-pass") {
		t.Fatalf("root 应 role=100 且 env 口令可登录，role=%d", u.Role)
	}

	created2, err := EnsureRootUser(st)
	if err != nil || created2 {
		t.Fatalf("二次应幂等跳过，created=%v err=%v", created2, err)
	}
}

// TestLoginLogoutFlow 登录成功签发 cookie（可过 root 探测路由）、错误口令 401、
// 未登录 401、登出下发清除 cookie。
func TestLoginLogoutFlow(t *testing.T) {
	r, st, _ := newTestAPI(t)
	if _, err := EnsureRootUser(st); err != nil {
		t.Fatalf("引导 root 失败: %v", err)
	}

	// 错误口令 → 401（与用户不存在同一错误信息）。
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": RootUsername, "password": "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，实际 %d", w.Code)
	}

	// 成功登录 → 200 + Set-Cookie；cookie 可过 root 探测路由。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": RootUsername, "password": DefaultRootPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	pair := sessionCookieFrom(t, w)
	w = doJSON(t, r, http.MethodGet, "/api/_probe_root", pair, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("root 会话应通过探测路由，实际 %d", w.Code)
	}

	// 无 cookie → 401。
	w = doJSON(t, r, http.MethodGet, "/api/_probe_root", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}

	// 登出 → 下发清除 cookie。
	w = doJSON(t, r, http.MethodPost, "/api/user/logout", pair, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d", w.Code)
	}
	sc := w.Header().Get("Set-Cookie")
	if sc == "" || !(strings.Contains(sc, "Max-Age=0") || strings.Contains(sc, "Max-Age=-1")) {
		t.Fatalf("登出应下发过期 cookie，实际 %q", sc)
	}
}

// TestAuthVersionInvalidatesSession auth_version 递增后，旧会话立即失效（401）。
func TestAuthVersionInvalidatesSession(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "alice", "alice-pass", model.RoleUser)

	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "alice", "password": "alice-pass"})
	pair := sessionCookieFrom(t, w)
	if w := doJSON(t, r, http.MethodGet, "/api/_probe_auth", pair, nil); w.Code != http.StatusOK {
		t.Fatalf("登录后会话应有效，实际 %d", w.Code)
	}

	// 模拟改密：auth_version 递增。
	if err := st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Update("auth_version", u.AuthVersion+1).Error; err != nil {
		t.Fatalf("递增 auth_version 失败: %v", err)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/_probe_auth", pair, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("auth_version 递增后旧会话应失效（401），实际 %d", w.Code)
	}

	// 重新登录获得新会话，恢复可用。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "alice", "password": "alice-pass"})
	pair2 := sessionCookieFrom(t, w)
	if w := doJSON(t, r, http.MethodGet, "/api/_probe_auth", pair2, nil); w.Code != http.StatusOK {
		t.Fatalf("重新登录后应恢复，实际 %d", w.Code)
	}
}

// TestRequireRootForbidden 普通用户：可过 RequireAuth，但 root 端点 403。
func TestRequireRootForbidden(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "bob", "bob-pass", model.RoleUser)

	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "bob", "password": "bob-pass"})
	pair := sessionCookieFrom(t, w)

	if w := doJSON(t, r, http.MethodGet, "/api/_probe_auth", pair, nil); w.Code != http.StatusOK {
		t.Fatalf("普通用户会话应通过 auth 探测，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/_probe_root", pair, nil); w.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问 root 端点应 403，实际 %d", w.Code)
	}
	// 被禁用用户：会话立即失效。
	if err := st.Write.Model(&model.User{}).Where("username = ?", "bob").
		Update("status", model.StatusDisabled).Error; err != nil {
		t.Fatalf("禁用用户失败: %v", err)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/_probe_auth", pair, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("被禁用用户会话应失效（401），实际 %d", w.Code)
	}
}
