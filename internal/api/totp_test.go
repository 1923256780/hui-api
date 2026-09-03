// totp_test.go 两步验证与二段式登录测试（M3-wave2，docs/05 §5.9）：
// TOTP setup/enable/disable 全链路、错码拒绝、二段式登录 stage1 会话防提权、
// 错码拒绝、disable 后免验、个人中心改密/改邮箱。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"

	"github.com/1923256780/hui-api/internal/model"
)

// mustCode 生成当前时刻的有效 TOTP 码（与被测端点同一实现库）。
func mustCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("生成 TOTP 码失败: %v", err)
	}
	return code
}

// loginAs 口令登录并返回会话 cookie。
func loginAs(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": username, "password": password})
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	return sessionCookieFrom(t, w)
}

// enableTOTPFor 走真实 setup+enable 流程为用户开启两步验证，返回 secret。
func enableTOTPFor(t *testing.T, r *gin.Engine, cookie string) string {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/api/user/totp/setup", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	data := dataMap(t, w.Body.String())
	secret, _ := data["secret"].(string)
	if secret == "" {
		t.Fatalf("setup 应返回 secret: %v", data)
	}
	uri, _ := data["otpauth_uri"].(string)
	if !strings.Contains(uri, "/totp/") || !strings.Contains(uri, "issuer=Hui%20Api") {
		t.Fatalf("otpauth URI 应含 issuer=Hui Api 与路径: %q", uri)
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/enable", cookie,
		map[string]string{"code": mustCode(t, secret)})
	if w.Code != http.StatusOK {
		t.Fatalf("enable 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	return secret
}

// TestTOTPRoundTrip setup→错码拒绝→enable→重复 setup 拒绝→disable 错码拒绝
// →disable 成功双列清空→再次 disable 拒绝。
func TestTOTPRoundTrip(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "alice", "alice-pass", model.RoleUser)
	cookie := loginAs(t, r, "alice", "alice-pass")

	secret := enableTOTPFor(t, r, cookie)
	var u model.User
	if err := st.Read.Where("username = ?", "alice").First(&u).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if !u.TOTPEnabled || u.TOTPSecret != secret {
		t.Fatalf("enable 后应落库 enabled+secret，实际 enabled=%v", u.TOTPEnabled)
	}

	// 已启用后重复 setup → 400 totp_already_enabled。
	w := doJSON(t, r, http.MethodPost, "/api/user/totp/setup", cookie, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "totp_already_enabled") {
		t.Fatalf("重复 setup 应 400 totp_already_enabled，实际 %d %s", w.Code, w.Body.String())
	}

	// disable 错码 → 400 totp_code_invalid（密钥保留，可重试）。
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
		map[string]string{"code": "000000"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "totp_code_invalid") {
		t.Fatalf("disable 错码应 400 totp_code_invalid，实际 %d %s", w.Code, w.Body.String())
	}

	// disable 正确码 → 200 且双列清空。
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
		map[string]string{"code": mustCode(t, secret)})
	if w.Code != http.StatusOK {
		t.Fatalf("disable 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var u2 model.User
	if err := st.Read.Where("username = ?", "alice").First(&u2).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if u2.TOTPEnabled || u2.TOTPSecret != "" {
		t.Fatalf("disable 后应双列清空，实际 enabled=%v secret-len=%d", u2.TOTPEnabled, len(u2.TOTPSecret))
	}

	// 未启用时 disable → 400 totp_not_enabled。
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
		map[string]string{"code": "123456"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "totp_not_enabled") {
		t.Fatalf("未启用 disable 应 400 totp_not_enabled，实际 %d %s", w.Code, w.Body.String())
	}
}

// TestTOTPEnableRequiresSetup 未 setup 直接 enable → 400 totp_not_setup；
// enable 错码 → 400 totp_code_invalid。
func TestTOTPEnableRequiresSetup(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "bob", "bob-pass", model.RoleUser)
	cookie := loginAs(t, r, "bob", "bob-pass")

	w := doJSON(t, r, http.MethodPost, "/api/user/totp/enable", cookie,
		map[string]string{"code": "123456"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "totp_not_setup") {
		t.Fatalf("未 setup enable 应 400 totp_not_setup，实际 %d %s", w.Code, w.Body.String())
	}

	// setup 后错码 enable → 400 且不启用。
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/setup", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup 应 200，实际 %d", w.Code)
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/enable", cookie,
		map[string]string{"code": "000000"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "totp_code_invalid") {
		t.Fatalf("enable 错码应 400 totp_code_invalid，实际 %d %s", w.Code, w.Body.String())
	}
	var u model.User
	if err := st.Read.Where("username = ?", "bob").First(&u).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if u.TOTPEnabled {
		t.Fatal("错码 enable 不应启用")
	}
}

// TestTwoStageLogin 二段式登录全链路：登录返回 require_2fa + stage1 会话 →
// stage1 会话访问自服务 401 totp_required（防提权）→ 错码 400 → 正确码签发
// 完整会话 → 自服务恢复。
func TestTwoStageLogin(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "carol", "carol-pass", model.RoleUser)
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "carol", "password": "carol-pass"})
	cookie := sessionCookieFrom(t, w)
	enableTOTPFor(t, r, cookie)

	// 登录 → require_2fa + 新 stage1 会话 cookie。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "carol", "password": "carol-pass"})
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d", w.Code)
	}
	var loginResp struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(w.Body.String()), &loginResp); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	if loginResp.Data["require_2fa"] != true {
		t.Fatalf("应返回 require_2fa:true: %v", loginResp.Data)
	}
	stage1 := sessionCookieFrom(t, w)

	// stage1 会话访问自服务 → 401 totp_required（防半登录态提权）。
	w = doJSON(t, r, http.MethodGet, "/api/user/self", stage1, nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "totp_required") {
		t.Fatalf("stage1 会话访问自服务应 401 totp_required，实际 %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/api/user/identities", stage1, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stage1 会话访问身份列表应 401，实际 %d", w.Code)
	}

	// 无会话调 2fa → 401；错码 → 400 totp_code_invalid。
	w = doJSON(t, r, http.MethodPost, "/api/user/login/2fa", "",
		map[string]string{"code": "123456"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无会话 2fa 应 401，实际 %d", w.Code)
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/login/2fa", stage1,
		map[string]string{"code": "000000"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "totp_code_invalid") {
		t.Fatalf("错码应 400 totp_code_invalid，实际 %d %s", w.Code, w.Body.String())
	}

	// 正确码 → 完整会话 + 用户信息；新会话访问自服务 200。
	var u model.User
	if err := st.Read.Where("username = ?", "carol").First(&u).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/login/2fa", stage1,
		map[string]string{"code": mustCode(t, u.TOTPSecret)})
	if w.Code != http.StatusOK {
		t.Fatalf("正确码应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	full := sessionCookieFrom(t, w)
	if w = doJSON(t, r, http.MethodGet, "/api/user/self", full, nil); w.Code != http.StatusOK {
		t.Fatalf("完整会话访问自服务应 200，实际 %d", w.Code)
	}
}

// TestTwoStageLoginAfterDisable disable 后登录免验：直接返回用户信息且
// 会话可直接访问自服务。
func TestTwoStageLoginAfterDisable(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "dave", "dave-pass", model.RoleUser)
	cookie := loginAs(t, r, "dave", "dave-pass")
	secret := enableTOTPFor(t, r, cookie)

	// 走 stage1 完成登录（改密类操作需完整会话……disable 本身要求登录态，
	// 用最初完整会话直接 disable）。
	w := doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
		map[string]string{"code": mustCode(t, secret)})
	if w.Code != http.StatusOK {
		t.Fatalf("disable 应 200，实际 %d", w.Code)
	}

	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "dave", "password": "dave-pass"})
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d", w.Code)
	}
	var loginResp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(w.Body.String()), &loginResp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if _, has := loginResp.Data["require_2fa"]; has {
		t.Fatalf("disable 后登录不应要求两步验证: %v", loginResp.Data)
	}
	pair := sessionCookieFrom(t, w)
	if w = doJSON(t, r, http.MethodGet, "/api/user/self", pair, nil); w.Code != http.StatusOK {
		t.Fatalf("登录后会话应直接可用，实际 %d", w.Code)
	}
}

// TestChangePasswordSelf 个人中心改密：旧口令错 → 400 old_password_mismatch；
// 正确 → 200 + 重签会话；旧会话失效（authv 递增）、新会话可用。
func TestChangePasswordSelf(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "erin", "erin-pass", model.RoleUser)
	oldCookie := loginAs(t, r, "erin", "erin-pass")

	w := doJSON(t, r, http.MethodPost, "/api/user/password", oldCookie,
		map[string]string{"old_password": "wrong-pass", "new_password": "new-pass-1"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "old_password_mismatch") {
		t.Fatalf("旧口令错应 400 old_password_mismatch，实际 %d %s", w.Code, w.Body.String())
	}

	w = doJSON(t, r, http.MethodPost, "/api/user/password", oldCookie,
		map[string]string{"old_password": "erin-pass", "new_password": "new-pass-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("改密应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	newCookie := sessionCookieFrom(t, w)

	// 旧会话失效（auth_version 递增），新会话可用；新口令可登录。
	if w = doJSON(t, r, http.MethodGet, "/api/user/self", oldCookie, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("改密后旧会话应失效，实际 %d", w.Code)
	}
	if w = doJSON(t, r, http.MethodGet, "/api/user/self", newCookie, nil); w.Code != http.StatusOK {
		t.Fatalf("重签会话应可用，实际 %d", w.Code)
	}
	if w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "erin", "password": "new-pass-1"}); w.Code != http.StatusOK {
		t.Fatalf("新口令应可登录，实际 %d", w.Code)
	}
}

// TestSetPasswordForOAuthAccount 无口令账号（OAuth 建户）首次设置口令
// 免验旧口令：直接设置成功并可登录。
func TestSetPasswordForOAuthAccount(t *testing.T) {
	r, st, h := newTestAPI(t)
	hash := "" // 模拟 OAuth 建户：无口令
	u := model.User{
		Username: "oauther", PasswordHash: hash, Role: model.RoleUser,
		Status: model.StatusEnabled, AuthVersion: 1, CreatedTime: time.Now().Unix(),
	}
	if err := st.Write.Create(&u).Error; err != nil {
		t.Fatalf("建户失败: %v", err)
	}
	// 测试内直接用会话管理器签发完整会话（等价登录态）。
	w := &httptest.ResponseRecorder{}
	c, _ := gin.CreateTestContext(w)
	h.sess.Issue(c, u.ID, u.AuthVersion)
	cookie := sessionCookieFrom(t, w)

	wr := doJSON(t, r, http.MethodPost, "/api/user/password", cookie,
		map[string]string{"old_password": "", "new_password": "set-pass-1"})
	if wr.Code != http.StatusOK {
		t.Fatalf("无口令账号首次设密应 200，实际 %d body=%s", wr.Code, wr.Body.String())
	}
	if w2 := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "oauther", "password": "set-pass-1"}); w2.Code != http.StatusOK {
		t.Fatalf("设置后的口令应可登录，实际 %d", w2.Code)
	}
}

// TestChangeEmailSelf 改邮箱：非法格式 400；成功 200；占用 409 email_conflict。
func TestChangeEmailSelf(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "frank", "frank-pass", model.RoleUser)
	seedUser(t, st, "grace", "grace-pass", model.RoleUser)
	if err := st.Write.Model(&model.User{}).Where("username = ?", "grace").
		Update("email", "grace@x.io").Error; err != nil {
		t.Fatalf("预备邮箱失败: %v", err)
	}
	cookie := loginAs(t, r, "frank", "frank-pass")

	w := doJSON(t, r, http.MethodPost, "/api/user/email", cookie,
		map[string]string{"email": "not-an-email"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法邮箱应 400，实际 %d", w.Code)
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/email", cookie,
		map[string]string{"email": "frank@x.io"})
	if w.Code != http.StatusOK {
		t.Fatalf("改邮箱应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/email", cookie,
		map[string]string{"email": "grace@x.io"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "email_conflict") {
		t.Fatalf("占用邮箱应 409 email_conflict，实际 %d %s", w.Code, w.Body.String())
	}
	var u model.User
	if err := st.Read.Where("username = ?", "frank").First(&u).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if u.Email != "frank@x.io" {
		t.Fatalf("邮箱应已更新，实际 %q", u.Email)
	}
}
