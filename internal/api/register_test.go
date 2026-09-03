// register_test.go 公开注册体系与 options 脱敏测试（M3-wave1，docs/05）：
// setup 能力发现、注册矩阵（开关/参数/查重/验证码/人机校验/邀请奖励）、
// IP 限频、验证码发送门控与限频、验证码重置口令、options 敏感值脱敏与哨兵。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/mailer"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/verification"
)

// ---- 测试替身 ----

// fakeTurnstile TurnstileVerifier 替身：记录调用次数，返回预设结果。
type fakeTurnstile struct {
	ok    bool
	err   error
	calls int
}

func (f *fakeTurnstile) Verify(_ context.Context, secret, token, ip string) (bool, error) {
	f.calls++
	return f.ok, f.err
}

// fakeMailer Mailer 替身：记录收件人/主题，返回预设错误。
type fakeMailer struct {
	tos     []string
	subject []string
	err     error
}

func (f *fakeMailer) Send(to, subject, body string) error {
	f.tos = append(f.tos, to)
	f.subject = append(f.subject, subject)
	return f.err
}

// ---- 测试工具 ----

// setOpts 写入运行轨键值并触发热加载（等价管理面 PUT 的生效语义）。
func setOpts(t *testing.T, h *Handler, kv map[string]string) {
	t.Helper()
	if err := h.st.SetOptions(kv); err != nil {
		t.Fatalf("写入 options 失败: %v", err)
	}
	if err := h.rt.Reload(); err != nil {
		t.Fatalf("热加载失败: %v", err)
	}
}

// enableRegister 打开注册并配置新户配额。
func enableRegister(t *testing.T, h *Handler) {
	t.Helper()
	setOpts(t, h, map[string]string{
		OptionKeyRegisterEnabled:         "true",
		OptionKeyRegisterQuotaForNewUser: "100",
	})
}

// registerBody 构造注册请求体。
func registerBody(username, email string, mutate func(map[string]any)) map[string]any {
	body := map[string]any{
		"username": username,
		"password": "pass-123456",
		"email":    email,
	}
	if mutate != nil {
		mutate(body)
	}
	return body
}

// dataMap 提取统一包裹结构的 data 对象。
func dataMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var resp struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, body)
	}
	return resp.Data
}

// doJSONIP 以指定 RemoteAddr IP 发起 JSON POST（并发注册测试为每请求
// 独立 IP，避免共享 httptest 默认地址触发同 IP 限频）。
func doJSONIP(t *testing.T, r *gin.Engine, method, path, ip string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- GET /api/setup ----

// TestSetupFlags 默认能力发现全关（OAuth wave2 前恒 false）；开关与
// Turnstile 配置后 site_key 仅在启用时下发。
func TestSetupFlags(t *testing.T) {
	r, st, h := newTestAPI(t)
	if _, err := EnsureRootUser(st); err != nil {
		t.Fatalf("引导 root 失败: %v", err)
	}

	w := doJSON(t, r, http.MethodGet, "/api/setup", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup 应 200，实际 %d", w.Code)
	}
	data := dataMap(t, w.Body.String())
	if data["register_enabled"] != false || data["email_verification"] != false {
		t.Fatalf("默认开关应全关: %v", data)
	}
	if data["turnstile_site_key"] != "" {
		t.Fatalf("未启用 Turnstile 不应下发 site_key: %v", data["turnstile_site_key"])
	}
	oauth, ok := data["oauth"].(map[string]any)
	if !ok || oauth["github"] != false || oauth["linuxdo"] != false || oauth["oidc"] != false {
		t.Fatalf("OAuth wave2 前应恒 false: %v", data["oauth"])
	}

	setOpts(t, h, map[string]string{
		OptionKeyRegisterEnabled:           "true",
		OptionKeyRegisterEmailVerification: "true",
		OptionKeyTurnstileEnabled:          "true",
		OptionKeyTurnstileSiteKey:          "site-123",
	})
	w = doJSON(t, r, http.MethodGet, "/api/setup", "", nil)
	data = dataMap(t, w.Body.String())
	if data["register_enabled"] != true || data["email_verification"] != true {
		t.Fatalf("开关应生效: %v", data)
	}
	if data["turnstile_site_key"] != "site-123" {
		t.Fatalf("启用后应下发 site_key: %v", data["turnstile_site_key"])
	}
}

// ---- POST /api/user/register ----

// TestRegisterDisabled 开关门控：未开放时 403 register_disabled 且无落库。
func TestRegisterDisabled(t *testing.T) {
	r, st, _ := newTestAPI(t)
	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("alice", "alice@example.com", nil))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "register_disabled") {
		t.Fatalf("未开放注册应 403 register_disabled，实际 %d body=%s", w.Code, w.Body.String())
	}
	var n int64
	_ = st.Read.Model(&model.User{}).Where("username = ?", "alice").Count(&n).Error
	if n != 0 {
		t.Fatal("拒绝注册不应落库")
	}
}

// TestRegisterSuccessAndDuplicate 正常注册 201：库内有户、配额、邀请码；
// username 与 email 重复分别 409。
func TestRegisterSuccessAndDuplicate(t *testing.T) {
	r, st, h := newTestAPI(t)
	enableRegister(t, h)

	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("alice", "alice@example.com", nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("注册应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	data := dataMap(t, w.Body.String())
	if data["username"] != "alice" || data["aff_code"] == "" || data["id"] == nil {
		t.Fatalf("响应应含 id/username/aff_code: %v", data)
	}

	var u model.User
	if err := st.Read.Where("username = ?", "alice").First(&u).Error; err != nil {
		t.Fatalf("用户应落库: %v", err)
	}
	if u.Quota != 100 || u.Email != "alice@example.com" || u.AffCode == "" {
		t.Fatalf("用户字段不符: quota=%d email=%q aff=%q", u.Quota, u.Email, u.AffCode)
	}
	if !strings.Contains(u.PasswordHash, "$2") {
		t.Fatalf("口令应经 bcrypt: %q", u.PasswordHash)
	}

	// username 重复 → 409 username_conflict。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("alice", "other@example.com", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "username_conflict") {
		t.Fatalf("重复用户名应 409 username_conflict，实际 %d body=%s", w.Code, w.Body.String())
	}
	// email 重复 → 409 email_conflict。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("alice2", "alice@example.com", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "email_conflict") {
		t.Fatalf("重复邮箱应 409 email_conflict，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestRegisterValidation 参数校验：缺字段、坏邮箱、短口令均 400。
func TestRegisterValidation(t *testing.T) {
	r, _, h := newTestAPI(t)
	enableRegister(t, h)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺用户名", map[string]any{"password": "x123456", "email": "a@b.c"}},
		{"缺口令", map[string]any{"username": "u", "email": "a@b.c"}},
		{"缺邮箱", map[string]any{"username": "u", "password": "x123456"}},
		{"坏邮箱", map[string]any{"username": "u", "password": "x123456", "email": "not-an-email"}},
		{"短口令", map[string]any{"username": "u", "password": "123", "email": "a@b.c"}},
	}
	for _, tc := range cases {
		w := doJSON(t, r, http.MethodPost, "/api/user/register", "", tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Fatalf("%s 应 400 invalid_request，实际 %d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
}

// TestRegisterWithVerification 邮箱验证码开启：无码/错码拒绝，正确码放行且一次性。
func TestRegisterWithVerification(t *testing.T) {
	r, _, h := newTestAPI(t)
	enableRegister(t, h)
	setOpts(t, h, map[string]string{OptionKeyRegisterEmailVerification: "true"})

	// 无码 → 400 code_invalid_or_expired。
	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("bob", "bob@example.com", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "code_invalid_or_expired") {
		t.Fatalf("无码应 400 code_invalid_or_expired，实际 %d body=%s", w.Code, w.Body.String())
	}

	code, err := h.vstore.Issue("bob@example.com", purposeRegister)
	if err != nil {
		t.Fatalf("签发验证码失败: %v", err)
	}
	// 错码 → 400 code_mismatch。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("bob", "bob@example.com", func(b map[string]any) { b["code"] = "000000" }))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "code_mismatch") {
		t.Fatalf("错码应 400 code_mismatch，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 正确码 → 201。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("bob", "bob@example.com", func(b map[string]any) { b["code"] = code }))
	if w.Code != http.StatusCreated {
		t.Fatalf("正确码应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 码一次性消费：重放失败（换新用户名也无法再用旧码）。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("bob2", "bob2@example.com", func(b map[string]any) { b["code"] = code }))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("重放已消费码应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestRegisterTurnstile 人机校验开启：mock 拒绝 → 400 turnstile_failed；
// mock 放行 → 201 且 verifier 恰被调用一次。
func TestRegisterTurnstile(t *testing.T) {
	r, _, h := newTestAPI(t)
	enableRegister(t, h)
	setOpts(t, h, map[string]string{
		OptionKeyTurnstileEnabled:   "true",
		OptionKeyTurnstileSiteKey:   "site-x",
		OptionKeyTurnstileSecretKey: "secret-x",
	})
	ft := &fakeTurnstile{ok: false}
	h.verifier = ft

	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("carol", "carol@example.com", func(b map[string]any) { b["turnstile_token"] = "tok" }))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "turnstile_failed") {
		t.Fatalf("人机校验拒绝应 400 turnstile_failed，实际 %d body=%s", w.Code, w.Body.String())
	}
	if ft.calls != 1 {
		t.Fatalf("verifier 应被调用 1 次，实际 %d", ft.calls)
	}

	ft.ok = true
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("carol", "carol@example.com", func(b map[string]any) { b["turnstile_token"] = "tok" }))
	if w.Code != http.StatusCreated {
		t.Fatalf("人机校验通过应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestRegisterAffReward 邀请奖励：双向入账、aff_history_quota 同步、两条
// protocol=aff 日志；无效邀请码不阻断注册也不发奖励。
func TestRegisterAffReward(t *testing.T) {
	r, st, h := newTestAPI(t)
	enableRegister(t, h)
	setOpts(t, h, map[string]string{
		OptionKeyRegisterQuotaForNewUser: "0",
		OptionKeyAffRewardInviter:        "1000",
		OptionKeyAffRewardInvitee:        "50",
	})

	// 邀请人注册（无邀请码）。
	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("inviter", "inv@example.com", nil))
	invAff, _ := dataMap(t, w.Body.String())["aff_code"].(string)
	inviterID, _ := dataMap(t, w.Body.String())["id"].(float64)

	// 被邀请人注册（带邀请码）。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("invitee", "inv2@example.com", func(b map[string]any) { b["aff_code"] = invAff }))
	if w.Code != http.StatusCreated {
		t.Fatalf("带邀请码注册应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	inviteeID, _ := dataMap(t, w.Body.String())["id"].(float64)

	var inviter model.User
	if err := st.Read.First(&inviter, int64(inviterID)).Error; err != nil {
		t.Fatalf("查询邀请人失败: %v", err)
	}
	var invitee model.User
	if err := st.Read.First(&invitee, int64(inviteeID)).Error; err != nil {
		t.Fatalf("查询被邀请人失败: %v", err)
	}
	if inviter.Quota != 1000 || inviter.AffHistoryQuota != 1000 {
		t.Fatalf("邀请人应入账 1000（quota/aff_history 同步），实际 %d/%d", inviter.Quota, inviter.AffHistoryQuota)
	}
	if invitee.Quota != 50 || invitee.InviterID != inviter.ID {
		t.Fatalf("被邀请人应 quota=50 且记录 InviterID，实际 quota=%d inviter=%d", invitee.Quota, invitee.InviterID)
	}
	var logs []model.Log
	if err := st.Read.Where("protocol = ?", "aff").Order("user_id").Find(&logs).Error; err != nil {
		t.Fatalf("查询 aff 日志失败: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("应恰有 2 条 aff 日志，实际 %d", len(logs))
	}
	if logs[0].UserID != inviter.ID || logs[0].Quota != 1000 || logs[0].ModelName != "register_reward_inviter" {
		t.Fatalf("邀请人日志不符: %+v", logs[0])
	}
	if logs[1].UserID != invitee.ID || logs[1].Quota != 50 || logs[1].ModelName != "register_reward_invitee" {
		t.Fatalf("被邀请人日志不符: %+v", logs[1])
	}

	// 无效邀请码：注册成功但无奖励、无日志。
	w = doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("lonely", "lonely@example.com", func(b map[string]any) { b["aff_code"] = "NOEXIST99" }))
	if w.Code != http.StatusCreated {
		t.Fatalf("无效邀请码不应阻断注册，实际 %d body=%s", w.Code, w.Body.String())
	}
	var n int64
	_ = st.Read.Model(&model.Log{}).Where("protocol = ?", "aff").Count(&n).Error
	if n != 2 {
		t.Fatalf("aff 日志应仍为 2 条，实际 %d", n)
	}
}

// TestRegisterAffConcurrent 并发注册恰一入账：SQLite 单写池串行化 +
// gorm.Expr 原地累加，邀请人收益恰为 N×reward，日志恰 N 条。
func TestRegisterAffConcurrent(t *testing.T) {
	r, st, h := newTestAPI(t)
	enableRegister(t, h)
	setOpts(t, h, map[string]string{
		OptionKeyRegisterQuotaForNewUser: "0",
		OptionKeyAffRewardInviter:        "1000",
		OptionKeyAffRewardInvitee:        "0",
	})

	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("inviter", "inv@example.com", nil))
	invAff := dataMap(t, w.Body.String())["aff_code"].(string)

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := registerBody("invitee"+fmt.Sprintf("%02d", i), fmt.Sprintf("invitee%02d@example.com", i),
				func(b map[string]any) { b["aff_code"] = invAff })
			w := doJSONIP(t, r, http.MethodPost, "/api/user/register", fmt.Sprintf("10.1.%d.%d", i/250, i%250+1), body)
			if w.Code != http.StatusCreated {
				t.Errorf("并发注册 #%d 应 201，实际 %d body=%s", i, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()

	var inviter model.User
	if err := st.Read.Where("username = ?", "inviter").First(&inviter).Error; err != nil {
		t.Fatalf("查询邀请人失败: %v", err)
	}
	if inviter.Quota != 1000*n || inviter.AffHistoryQuota != 1000*n {
		t.Fatalf("并发注册后邀请人应恰得 %d（实际 quota=%d aff_history=%d）", 1000*n, inviter.Quota, inviter.AffHistoryQuota)
	}
	var cnt int64
	_ = st.Read.Model(&model.Log{}).Where("protocol = ? AND user_id = ?", "aff", inviter.ID).Count(&cnt).Error
	if cnt != n {
		t.Fatalf("邀请人 aff 日志应恰 %d 条，实际 %d", n, cnt)
	}
}

// TestRegisterIPLimit 注册 IP 限频：1h 窗口 5 次后 429 且带 Retry-After。
func TestRegisterIPLimit(t *testing.T) {
	r, _, h := newTestAPI(t)
	enableRegister(t, h)
	setOpts(t, h, map[string]string{OptionKeyRegisterQuotaForNewUser: "0"})

	for i := 0; i < registerIPLimitMax; i++ {
		w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
			registerBody("user"+string(rune('a'+i)), "u"+string(rune('a'+i))+"@example.com", nil))
		if w.Code != http.StatusCreated {
			t.Fatalf("第 %d 次注册应 201，实际 %d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("blocked", "blocked@example.com", nil))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("超限应 429 rate_limited，实际 %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应携带 Retry-After 头")
	}
}

// ---- POST /api/verification_code ----

// TestSendVerificationCodeGate SMTP 未启用 → 503 smtp_not_configured；
// purpose 非法 → 400。
func TestSendVerificationCodeGate(t *testing.T) {
	r, _, h := newTestAPI(t)
	w := doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "a@b.c", "purpose": "register"})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "smtp_not_configured") {
		t.Fatalf("SMTP 未启用应 503 smtp_not_configured，实际 %d body=%s", w.Code, w.Body.String())
	}
	setOpts(t, h, map[string]string{mailer.KeyEnabled: "true"})
	w = doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "a@b.c", "purpose": "bogus"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("purpose 非法应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestSendVerificationCodeFlow 发码流程：mock mailer 收信、条目入库可校验、
// 60s 限频内重发 429 且不再调用 mailer。
func TestSendVerificationCodeFlow(t *testing.T) {
	r, _, h := newTestAPI(t)
	setOpts(t, h, map[string]string{mailer.KeyEnabled: "true"})
	fm := &fakeMailer{}
	h.mailer = fm

	w := doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "flow@example.com", "purpose": "register"})
	if w.Code != http.StatusOK {
		t.Fatalf("发码应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if data := dataMap(t, w.Body.String()); data["sent"] != true {
		t.Fatalf("应返回 sent=true: %v", data)
	}
	if len(fm.tos) != 1 || fm.tos[0] != "flow@example.com" {
		t.Fatalf("mailer 应收到收件人: %v", fm.tos)
	}
	if !strings.Contains(fm.subject[0], "验证码") {
		t.Fatalf("主题应含验证码标识: %q", fm.subject[0])
	}
	// 条目已入库（错码 ErrMismatch 而非 ErrNotFound 证明存在）。
	if err := h.vstore.Verify("flow@example.com", purposeRegister, "000000"); err != verification.ErrMismatch {
		t.Fatalf("发码后条目应存在（ErrMismatch），实际 %v", err)
	}
	// 60s 限频内重发 → 429，mailer 不再被调用。
	w = doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "flow@example.com", "purpose": "register"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("重发应 429，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(fm.tos) != 1 {
		t.Fatalf("限频内 mailer 不应再被调用，实际 %d 次", len(fm.tos))
	}
}

// ---- POST /api/user/reset_password ----

// TestResetPasswordFlow 验证码重置口令：重置后新密码可登录、旧密码拒绝、
// auth_version 递增；不存在邮箱 404；码一次性。
func TestResetPasswordFlow(t *testing.T) {
	r, st, h := newTestAPI(t)
	enableRegister(t, h)

	// 造一个带邮箱的用户。
	w := doJSON(t, r, http.MethodPost, "/api/user/register", "",
		registerBody("resetter", "reset@example.com", nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("注册失败: %d body=%s", w.Code, w.Body.String())
	}

	code, err := h.vstore.Issue("reset@example.com", purposeReset)
	if err != nil {
		t.Fatalf("签发重置码失败: %v", err)
	}
	// 错码拒绝。
	w = doJSON(t, r, http.MethodPost, "/api/user/reset_password", "",
		map[string]string{"email": "reset@example.com", "code": "000000", "new_password": "new-pass-123"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("错码应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 正确码重置。
	w = doJSON(t, r, http.MethodPost, "/api/user/reset_password", "",
		map[string]string{"email": "reset@example.com", "code": code, "new_password": "new-pass-123"})
	if w.Code != http.StatusOK {
		t.Fatalf("重置应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	// auth_version 递增。
	var u model.User
	if err := st.Read.Where("email = ?", "reset@example.com").First(&u).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if u.AuthVersion != 2 {
		t.Fatalf("auth_version 应递增为 2，实际 %d", u.AuthVersion)
	}
	// 新密码可登录、旧密码拒绝。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "resetter", "password": "new-pass-123"})
	if w.Code != http.StatusOK {
		t.Fatalf("新密码应可登录，实际 %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "resetter", "password": "pass-123456"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应拒绝，实际 %d", w.Code)
	}
	// 码一次性：重放 400。
	w = doJSON(t, r, http.MethodPost, "/api/user/reset_password", "",
		map[string]string{"email": "reset@example.com", "code": code, "new_password": "new-pass-456"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("重放已消费码应 400，实际 %d", w.Code)
	}
	// 不存在邮箱：码先签发后消费，用户缺失 → 404。
	missing, _ := h.vstore.Issue("ghost@example.com", purposeReset)
	w = doJSON(t, r, http.MethodPost, "/api/user/reset_password", "",
		map[string]string{"email": "ghost@example.com", "code": missing, "new_password": "new-pass-789"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知邮箱应 404，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// ---- Login IP 限频 ----

// TestLoginIPLimit 登录 IP 限频：1h 窗口 10 次尝试后 429（含失败计数）。
func TestLoginIPLimit(t *testing.T) {
	r, _, _ := newTestAPI(t)
	for i := 0; i < loginIPLimitMax; i++ {
		w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
			map[string]string{"username": "nobody", "password": "wrong"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败登录应 401，实际 %d", i+1, w.Code)
		}
	}
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "nobody", "password": "wrong"})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("超限登录应 429 rate_limited，实际 %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应携带 Retry-After 头")
	}
}

// ---- options 脱敏与哨兵 ----

// TestOptionMaskingAndSentinel 敏感键出口脱敏为 ******；PUT 哨兵值跳过不覆盖
//（库内旧值保留），非哨兵值正常写入；白名单新前缀可写、内部键拒写。
func TestOptionMaskingAndSentinel(t *testing.T) {
	r, st, h := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	// 写入敏感键与普通键。
	w := doJSON(t, r, http.MethodPut, "/api/option", cookie, map[string]any{
		"options": map[string]string{
			"smtp.password": "s3cret-pass",
			"smtp.host":     "smtp.example.com",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("写入应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 列表：敏感值脱敏，普通值明文。
	w = doJSON(t, r, http.MethodGet, "/api/option", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", w.Code)
	}
	var resp struct {
		Data struct {
			Items []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	masked := map[string]string{}
	for _, it := range resp.Data.Items {
		masked[it.Key] = it.Value
	}
	if masked["smtp.password"] != "******" {
		t.Fatalf("smtp.password 应脱敏为 ******，实际 %q", masked["smtp.password"])
	}
	if masked["smtp.host"] != "smtp.example.com" {
		t.Fatalf("smtp.host 应明文，实际 %q", masked["smtp.host"])
	}

	// 哨兵回写：smtp.password 保持旧值，smtp.host 正常更新；顺带验证白名单前缀。
	w = doJSON(t, r, http.MethodPut, "/api/option", cookie, map[string]any{
		"options": map[string]string{
			"smtp.password": "******",
			"smtp.host":     "smtp2.example.com",
			"smtp.username": "mailer-bot",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("哨兵回写应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var pw, host string
	var row model.Option
	if err := st.Read.Where("`key` = ?", "smtp.password").First(&row).Error; err != nil {
		t.Fatalf("查询 smtp.password 失败: %v", err)
	}
	pw = row.Value
	// 重置后复用：GORM 复用已填充主键的 struct 会附加旧主键条件。
	row = model.Option{}
	if err := st.Read.Where("`key` = ?", "smtp.host").First(&row).Error; err != nil {
		t.Fatalf("查询 smtp.host 失败: %v", err)
	}
	host = row.Value
	if pw != "s3cret-pass" {
		t.Fatalf("哨兵不应覆盖库内旧值，实际 %q", pw)
	}
	if host != "smtp2.example.com" {
		t.Fatalf("非哨兵键应正常更新，实际 %q", host)
	}
	if v, _ := h.rt.Get("smtp.password"); v != "s3cret-pass" {
		t.Fatalf("热加载后旧值应保留，实际 %q", v)
	}

	// 白名单：内部键拒写（回归），M3 新前缀可写。
	w = doJSON(t, r, http.MethodPut, "/api/option", cookie, map[string]any{
		"options": map[string]string{"schema_version": "99"},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "option_forbidden") {
		t.Fatalf("内部键应拒写，实际 %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPut, "/api/option", cookie, map[string]any{
		"options": map[string]string{
			"register.enabled":            "true",
			"aff.register_reward_inviter": "1000",
			"topup.rate":                  "1",
			"stripe.secret_key":           "sk_live_x",
			"epay.partner_id":             "p1",
			"oauth.github_client_id":      "gh1",
			"turnstile.secret_key":        "ts",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("M3 前缀应可写，实际 %d body=%s", w.Code, w.Body.String())
	}
}
