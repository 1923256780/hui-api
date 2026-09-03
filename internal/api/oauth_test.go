// oauth_test.go 第三方登录与身份绑定测试（M3-wave2，docs/05 §5.8）：
// httptest 假 provider（token/userinfo/发现文档）、state CSRF 篡改拒绝、
// 自动建户事务（撞名回滚）、绑定/解绑防锁死、OIDC 发现缓存、setup flags。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// fakeGithub 是 httptest 假 GitHub provider：token 端点记录收到的
// redirect_uri，userinfo 返回固定 id/email。authorize 端点仅用于断言
// 302 Location 前缀（服务端不会真正访问）。
type fakeGithub struct {
	ts          *httptest.Server
	mu          sync.Mutex
	tokenForms  []url.Values
	userHeaders []string
}

func newFakeGithub(t *testing.T, remoteUID int64, email string) *fakeGithub {
	t.Helper()
	f := &fakeGithub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析 token 表单失败: %v", err)
		}
		f.mu.Lock()
		f.tokenForms = append(f.tokenForms, r.PostForm)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tk-1","token_type":"bearer"}`))
	})
	mux.HandleFunc("/api/github/user", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.userHeaders = append(f.userHeaders, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d,"login":"gh-user","email":%q}`, remoteUID, email)
	})
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

// configureGithub 写入 provider 配置并把端点指向假服务器。
func configureGithub(t *testing.T, h *Handler, f *fakeGithub) {
	t.Helper()
	setOpts(t, h, map[string]string{
		OptionKeyOAuthGitHubClientID:     "cid-1",
		OptionKeyOAuthGitHubClientSecret: "csec-1",
	})
	h.oauthGithubAuthorizeURL = f.ts.URL + "/login/oauth/authorize"
	h.oauthGithubTokenURL = f.ts.URL + "/login/oauth/access_token"
	h.oauthGithubUserinfoURL = f.ts.URL + "/api/github/user"
}

// oauthDo 发起一次无 body 请求（可带 Cookie 与 RemoteAddr）。
func oauthDo(t *testing.T, r *gin.Engine, method, path, cookie, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if ip != "" {
		req.RemoteAddr = ip + ":1234"
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// oauthStateCookieOf 从响应 Cookies 提取 oauth_state cookie 值。
func oauthStateCookieOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, ck := range w.Result().Cookies() {
		if ck.Name == oauthStateCookie {
			return ck.Value
		}
	}
	t.Fatal("响应应携带 oauth_state cookie")
	return ""
}

// oauthRunFlow 走完整 authorize→callback 流程，返回 callback 响应。
// extraCookie 拼进 callback 请求（绑定模式传会话 cookie）。
func oauthRunFlow(t *testing.T, r *gin.Engine, provider, stateCookieValue, extraCookie, ip string) *httptest.ResponseRecorder {
	t.Helper()
	parts := strings.SplitN(stateCookieValue, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("state cookie 值应为 state|mode|uid: %q", stateCookieValue)
	}
	state := parts[0]
	cookie := oauthStateCookie + "=" + stateCookieValue
	if extraCookie != "" {
		cookie += "; " + extraCookie
	}
	return oauthDo(t, r, http.MethodGet,
		"/api/oauth/"+provider+"/callback?code=auth-code-1&state="+url.QueryEscape(state),
		cookie, ip)
}

// TestOAuthAuthorizeGate 未配置/未知 provider 404 oauth_not_configured；
// 配置后 302 且 Location 携带 client_id/scope/state/redirect_uri，cookie
// HttpOnly + 短 TTL；X-Forwarded-Proto https 时 redirect_uri 为 https。
func TestOAuthAuthorizeGate(t *testing.T) {
	r, st, h := newTestAPI(t)
	_, _ = st, h

	// 未配置。
	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "oauth_not_configured") {
		t.Fatalf("未配置应 404 oauth_not_configured，实际 %d %s", w.Code, w.Body.String())
	}
	// 未知 provider 名单外。
	w = oauthDo(t, r, http.MethodGet, "/api/oauth/evil", "", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("名单外 provider 应 404，实际 %d", w.Code)
	}

	f := newFakeGithub(t, 123, "gh@x.io")
	configureGithub(t, h, f)
	w = oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	if w.Code != http.StatusFound {
		t.Fatalf("已配置应 302，实际 %d", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("解析 Location 失败: %v", err)
	}
	if !strings.HasPrefix(loc.String(), f.ts.URL+"/login/oauth/authorize") {
		t.Fatalf("Location 应指向假 authorize 端点: %q", loc.String())
	}
	q := loc.Query()
	if q.Get("client_id") != "cid-1" || q.Get("scope") != "read:user" || q.Get("response_type") != "code" {
		t.Fatalf("authorize 参数不全: %v", q)
	}
	if q.Get("state") == "" || len(q.Get("state")) != 32 {
		t.Fatalf("state 应为 32 hex: %q", q.Get("state"))
	}
	if got := q.Get("redirect_uri"); !strings.HasPrefix(got, "http://") ||
		!strings.HasSuffix(got, "/api/oauth/github/callback") {
		t.Fatalf("redirect_uri 应为 http scheme + callback 路径: %q", got)
	}
	ck := w.Result().Cookies()
	httpOnly := false
	for _, c := range ck {
		if c.Name == oauthStateCookie {
			httpOnly = c.HttpOnly
		}
	}
	if !httpOnly {
		t.Fatal("state cookie 应 HttpOnly")
	}

	// X-Forwarded-Proto 信任（反代 https）。
	w = oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/github", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	loc2, _ := url.Parse(w2.Header().Get("Location"))
	if !strings.HasPrefix(loc2.Query().Get("redirect_uri"), "https://") {
		t.Fatalf("X-Forwarded-Proto https 时 redirect_uri 应为 https: %v", loc2.Query())
	}
}

// TestOAuthCallbackStateCSRF state 篡改/缺失一律 302 /login?oauth_failed=1
// 且无任何落库与会话签发。
func TestOAuthCallbackStateCSRF(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 123, "gh@x.io")
	configureGithub(t, h, f)

	// authorize 获取合法 state cookie。
	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie := oauthStateCookieOf(t, w)
	state := strings.SplitN(stateCookie, "|", 2)[0]

	// 篡改 state。
	failPath := "/login?oauth_failed=1"
	w = oauthDo(t, r, http.MethodGet,
		"/api/oauth/github/callback?code=x&state="+strings.Repeat("0", 32),
		oauthStateCookie+"="+stateCookie, "")
	if w.Code != http.StatusFound || w.Header().Get("Location") != failPath {
		t.Fatalf("篡改 state 应 302 失败路径，实际 %d %s", w.Code, w.Header().Get("Location"))
	}
	// 无 cookie。
	w = oauthDo(t, r, http.MethodGet,
		"/api/oauth/github/callback?code=x&state="+state, "", "")
	if w.Header().Get("Location") != failPath {
		t.Fatalf("无 state cookie 应拒绝，实际 %s", w.Header().Get("Location"))
	}
	// state cookie 值缺段。
	w = oauthDo(t, r, http.MethodGet,
		"/api/oauth/github/callback?code=x&state="+state,
		oauthStateCookie+"=onlystate", "")
	if w.Header().Get("Location") != failPath {
		t.Fatalf("缺段 cookie 应拒绝，实际 %s", w.Header().Get("Location"))
	}
	// 无落库。
	var n int64
	if err := st.Read.Model(&model.User{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("CSRF 拒绝不应建户，实际 n=%d err=%v", n, err)
	}
}

// TestOAuthAutoRegister 未绑定 + 注册开放 → 事务自动建户 + 身份绑定 +
// 完整会话 302 /console；新会话可用；token 端点收到正确 redirect_uri。
func TestOAuthAutoRegister(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 123, "gh@x.io")
	configureGithub(t, h, f)
	setOpts(t, h, map[string]string{
		OptionKeyRegisterEnabled:         "true",
		OptionKeyRegisterQuotaForNewUser: "100",
	})

	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie, "", "")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/console" {
		t.Fatalf("自动建户后应 302 /console，实际 %d %s", w.Code, w.Header().Get("Location"))
	}
	var u model.User
	if err := st.Read.Where("username = ?", "github_123").First(&u).Error; err != nil {
		t.Fatalf("应自动建户 github_123: %v", err)
	}
	if u.Quota != 100 || u.PasswordHash != "" || u.Email != "gh@x.io" || len(u.AffCode) != 8 {
		t.Fatalf("建户字段不符: quota=%d hash-len=%d email=%q aff=%q",
			u.Quota, len(u.PasswordHash), u.Email, u.AffCode)
	}
	var ident model.UserIdentity
	if err := st.Read.Where("provider = ? AND provider_uid = ?", "github", "123").
		First(&ident).Error; err != nil || ident.UserID != u.ID {
		t.Fatalf("应落库身份绑定: err=%v ident=%+v", err, ident)
	}
	// 回调签发的完整会话可用。
	sessCookie := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			sessCookie = c.Name + "=" + c.Value
		}
	}
	if sessCookie == "" {
		t.Fatal("回调应签发会话 cookie")
	}
	w = oauthDo(t, r, http.MethodGet, "/api/user/self", sessCookie, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "github_123") {
		t.Fatalf("OAuth 会话应可访问自服务，实际 %d %s", w.Code, w.Body.String())
	}
	// token 端点收到的表单。
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenForms) != 1 {
		t.Fatalf("token 端点应恰好收到一次请求，实际 %d", len(f.tokenForms))
	}
	form := f.tokenForms[0]
	if form.Get("code") != "auth-code-1" || form.Get("grant_type") != "authorization_code" ||
		form.Get("client_id") != "cid-1" || form.Get("client_secret") != "csec-1" {
		t.Fatalf("token 表单字段不符: %v", form)
	}
	if !strings.HasSuffix(form.Get("redirect_uri"), "/api/oauth/github/callback") {
		t.Fatalf("redirect_uri 应指向 callback: %q", form.Get("redirect_uri"))
	}
}

// TestOAuthAutoRegisterUsernameConflict 撞名回滚：username=<provider>_<uid>
// 已存在 → 整体 302 失败且不残留 user/identity（事务原子）。
func TestOAuthAutoRegisterUsernameConflict(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 999, "gh@x.io")
	configureGithub(t, h, f)
	setOpts(t, h, map[string]string{OptionKeyRegisterEnabled: "true"})
	// 预置同名用户。
	seedUser(t, st, "github_999", "x-pass", model.RoleUser)

	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie, "", "")
	if w.Header().Get("Location") != "/login?oauth_failed=1" {
		t.Fatalf("撞名应 302 失败路径，实际 %s", w.Header().Get("Location"))
	}
	var nU, nI int64
	_ = st.Read.Model(&model.User{}).Where("username = ?", "github_999").Count(&nU)
	_ = st.Read.Model(&model.UserIdentity{}).Count(&nI)
	if nU != 1 || nI != 0 {
		t.Fatalf("事务回滚后不应有新增身份/用户，users=%d identities=%d", nU, nI)
	}
}

// TestOAuthRegisterDisabled 未绑定 + 注册关闭 → 302 失败且不建户。
func TestOAuthRegisterDisabled(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 123, "gh@x.io")
	configureGithub(t, h, f)

	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie, "", "")
	if w.Header().Get("Location") != "/login?oauth_failed=1" {
		t.Fatalf("注册关闭应 302 失败路径，实际 %s", w.Header().Get("Location"))
	}
	var n int64
	_ = st.Read.Model(&model.User{}).Count(&n)
	if n != 0 {
		t.Fatalf("不应建户，实际 %d", n)
	}
}

// TestOAuthLoginExisting 命中身份：启用用户 → 完整会话 302 /console；
// 禁用用户 → 失败路径。
func TestOAuthLoginExisting(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 555, "")
	configureGithub(t, h, f)

	u := seedUser(t, st, "bound", "bound-pass", model.RoleUser)
	if err := st.Write.Create(&model.UserIdentity{
		UserID: u.ID, Provider: "github", ProviderUID: "555", CreatedTime: time.Now().Unix(),
	}).Error; err != nil {
		t.Fatalf("预置身份失败: %v", err)
	}

	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie, "", "")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/console" {
		t.Fatalf("命中身份应 302 /console，实际 %d %s", w.Code, w.Header().Get("Location"))
	}
	sessCookie := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			sessCookie = c.Name + "=" + c.Value
		}
	}
	if w = oauthDo(t, r, http.MethodGet, "/api/user/self", sessCookie, ""); w.Code != http.StatusOK {
		t.Fatalf("OAuth 登录会话应可用，实际 %d", w.Code)
	}

	// 禁用后同身份登录 → 失败。
	if err := st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Update("status", model.StatusDisabled).Error; err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	w = oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie = oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie, "", "")
	if w.Header().Get("Location") != "/login?oauth_failed=1" {
		t.Fatalf("禁用用户应拒绝，实际 %s", w.Header().Get("Location"))
	}
}

// TestOAuthBindFlow 绑定模式：登录态发起 bind → callback 携会话绑定成功
// 302 /console/profile；同一外部身份被他人已绑 → 失败路径。
func TestOAuthBindFlow(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 777, "")
	configureGithub(t, h, f)
	u := seedUser(t, st, "me", "me-pass", model.RoleUser)

	cookie := loginAs(t, r, "me", "me-pass")
	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github/bind", cookie, "")
	if w.Code != http.StatusFound {
		t.Fatalf("bind 发起应 302，实际 %d", w.Code)
	}
	stateCookie := oauthStateCookieOf(t, w)
	if !strings.HasSuffix(stateCookie, "|bind|"+fmt.Sprint(u.ID)) {
		t.Fatalf("bind 模式 state cookie 应标记 bind+uid: %q", stateCookie)
	}
	// 无会话发起 bind → 401。
	if w = oauthDo(t, r, http.MethodGet, "/api/oauth/github/bind", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 bind 应 401，实际 %d", w.Code)
	}

	// callback + 会话 → 绑定成功。
	w = oauthRunFlow(t, r, "github", stateCookie, cookie, "")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/console/profile" {
		t.Fatalf("绑定成功应 302 /console/profile，实际 %d %s", w.Code, w.Header().Get("Location"))
	}
	var ident model.UserIdentity
	if err := st.Read.Where("user_id = ? AND provider = ?", u.ID, "github").
		First(&ident).Error; err != nil || ident.ProviderUID != "777" {
		t.Fatalf("应落库绑定: err=%v ident=%+v", err, ident)
	}

	// 同一外部身份再绑给别人 → 复合唯一冲突 → 失败路径。
	u2 := seedUser(t, st, "me2", "me2-pass", model.RoleUser)
	_ = u2
	cookie2 := loginAs(t, r, "me2", "me2-pass")
	w = oauthDo(t, r, http.MethodGet, "/api/oauth/github/bind", cookie2, "")
	stateCookie2 := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie2, cookie2, "")
	if w.Header().Get("Location") != "/login?oauth_failed=1" {
		t.Fatalf("重复绑定应失败，实际 %s", w.Header().Get("Location"))
	}
}

// TestOAuthIdentityListAndLockout 身份列表/解绑：他人身份 404；无口令用户
// 最后一个身份拒绝解绑（identity_last）；设置口令后可解绑。
func TestOAuthIdentityListAndLockout(t *testing.T) {
	r, st, h := newTestAPI(t)
	f := newFakeGithub(t, 321, "nolib@x.io")
	configureGithub(t, h, f)
	setOpts(t, h, map[string]string{
		OptionKeyRegisterEnabled:         "true",
		OptionKeyRegisterQuotaForNewUser: "0",
	})

	// 自动建户（无口令 + 唯一身份）。
	w := oauthDo(t, r, http.MethodGet, "/api/oauth/github", "", "")
	stateCookie := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "github", stateCookie, "", "")
	sessCookie := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			sessCookie = c.Name + "=" + c.Value
		}
	}
	var ou model.User
	if err := st.Read.Where("username = ?", "github_321").First(&ou).Error; err != nil {
		t.Fatalf("自动建户失败: %v", err)
	}

	// 列表。
	w = oauthDo(t, r, http.MethodGet, "/api/user/identities", sessCookie, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"provider":"github"`) {
		t.Fatalf("身份列表应含 github: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data struct {
			Items []model.UserIdentity `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(w.Body.String()), &listResp); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(listResp.Data.Items) != 1 {
		t.Fatalf("应恰好一条身份，实际 %d", len(listResp.Data.Items))
	}
	identID := listResp.Data.Items[0].ID

	// 无口令 + 最后一个身份 → 400 identity_last。
	w = oauthDo(t, r, http.MethodDelete, fmt.Sprintf("/api/user/identities/%d", identID), sessCookie, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "identity_last") {
		t.Fatalf("最后一个身份应拒绝解绑，实际 %d %s", w.Code, w.Body.String())
	}

	// 他人身份 404（root 的 id=1 不存在，用大 id）。
	w = oauthDo(t, r, http.MethodDelete, "/api/user/identities/99999", sessCookie, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("他人/不存在身份应 404，实际 %d", w.Code)
	}

	// 设置口令后可解绑。
	w = doJSON(t, r, http.MethodPost, "/api/user/password", sessCookie,
		map[string]string{"old_password": "", "new_password": "fresh-pass-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("设置口令应 200，实际 %d", w.Code)
	}
	// 改密重签了会话。
	newCookie := sessionCookieFrom(t, w)
	w = oauthDo(t, r, http.MethodDelete, fmt.Sprintf("/api/user/identities/%d", identID), newCookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("设置口令后应可解绑，实际 %d body=%s", w.Code, w.Body.String())
	}
	var n int64
	_ = st.Read.Model(&model.UserIdentity{}).Where("user_id = ?", ou.ID).Count(&n)
	if n != 0 {
		t.Fatalf("解绑后不应残留，实际 %d", n)
	}
}

// TestOAuthOIDCDiscovery 假 OIDC issuer：发现文档解析 + 缓存（authorize 与
// callback 两次 resolve 只抓一次）+ sub 提取 + 自动建户。
func TestOAuthOIDCDiscovery(t *testing.T) {
	r, st, h := newTestAPI(t)
	var mu sync.Mutex
	discoveryHits := 0
	var issuerURL string // 先声明后赋值：闭包在请求时执行，此时已指向测试服务器
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		discoveryHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"authorization_endpoint":%q,"token_endpoint":%q,"userinfo_endpoint":%q}`,
			issuerURL+"/oidc/auth", issuerURL+"/oidc/token", issuerURL+"/oidc/userinfo")
	})
	mux.HandleFunc("/oidc/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"oidc-tk"}`))
	})
	mux.HandleFunc("/oidc/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":"sub-77","email":"oidc@x.io"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	issuerURL = ts.URL

	setOpts(t, h, map[string]string{
		OptionKeyOAuthOIDCClientID:       "oidc-cid",
		OptionKeyOAuthOIDCClientSecret:   "oidc-csec",
		OptionKeyOAuthOIDCIssuer:         ts.URL,
		OptionKeyRegisterEnabled:         "true",
		OptionKeyRegisterQuotaForNewUser: "50",
	})

	w := oauthDo(t, r, http.MethodGet, "/api/oauth/oidc", "", "")
	if w.Code != http.StatusFound {
		t.Fatalf("oidc 发起应 302，实际 %d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if !strings.HasPrefix(loc.String(), ts.URL+"/oidc/auth") {
		t.Fatalf("authorize 应来自发现文档: %q", loc.String())
	}
	stateCookie := oauthStateCookieOf(t, w)
	w = oauthRunFlow(t, r, "oidc", stateCookie, "", "")
	if w.Header().Get("Location") != "/console" {
		t.Fatalf("oidc 自动建户后应 302 /console，实际 %s", w.Header().Get("Location"))
	}
	var u model.User
	if err := st.Read.Where("username = ?", "oidc_sub-77").First(&u).Error; err != nil {
		t.Fatalf("应按 sub 建户 oidc_sub-77: %v", err)
	}
	if u.Quota != 50 {
		t.Fatalf("新户配额应 50，实际 %d", u.Quota)
	}
	mu.Lock()
	defer mu.Unlock()
	if discoveryHits != 1 {
		t.Fatalf("发现文档应缓存命中（authorize+callback 仅抓一次），实际 %d", discoveryHits)
	}
}

// TestSetupOAuthFlagsFlags /api/setup oauth 块按配置探测。
func TestSetupOAuthFlagsFlags(t *testing.T) {
	r, _, h := newTestAPI(t)
	setOpts(t, h, map[string]string{
		OptionKeyOAuthGitHubClientID:     "cid",
		OptionKeyOAuthGitHubClientSecret: "csec",
	})
	w := oauthDo(t, r, http.MethodGet, "/api/setup", "", "")
	data := dataMap(t, w.Body.String())
	oauth, _ := data["oauth"].(map[string]any)
	if oauth["github"] != true || oauth["linuxdo"] != false || oauth["oidc"] != false {
		t.Fatalf("setup oauth 块应按配置探测: %v", oauth)
	}
}
