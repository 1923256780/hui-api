// auth_limit_test.go 认证限频失败记账语义测试（M4 评审 M-B/M-C/M-D，docs/05）：
// 登录成功零消耗（NAT 共出口可用性）、2FA 二段式完整登录不双耗预算、
// 连续失败受限、发码 sendcode|IP 独立预算与 Turnstile 强制、TOTP 错码预算。
package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/1923256780/hui-api/internal/mailer"
	"github.com/1923256780/hui-api/internal/model"
)

// TestLoginSuccessNoBudget M-B 核心回归：同 IP 成功登录 N 次（N > 上限）不 429
// ——失败记账语义下成功响应零消耗；上限经 auth.login_ip_limit 可配生效。
func TestLoginSuccessNoBudget(t *testing.T) {
	r, st, h := newTestAPI(t)
	setOpts(t, h, map[string]string{OptionKeyAuthLoginIPLimit: "5"})
	seedUser(t, st, "alice", "alice-pass", model.RoleUser)

	for i := 0; i < 15; i++ {
		w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
			map[string]string{"username": "alice", "password": "alice-pass"})
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次成功登录应 200（成功不消耗预算），实际 %d body=%s", i+1, w.Code, w.Body.String())
		}
	}
}

// TestLoginIPLimitConfigurable M-B ③：auth.login_ip_limit 经管理面白名单可写
// 且非法值回退缺省（不归零、不放飞）。
func TestLoginIPLimitConfigurable(t *testing.T) {
	r, st, h := newTestAPI(t)
	cookie := loginRoot(t, r, st)
	w := doJSON(t, r, http.MethodPut, "/api/option", cookie, map[string]any{
		"options": map[string]string{"auth.login_ip_limit": "7"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("auth.login_ip_limit 应可写，实际 %d body=%s", w.Code, w.Body.String())
	}
	if v, _ := h.rt.Get(OptionKeyAuthLoginIPLimit); v != "7" {
		t.Fatalf("配置应热加载生效，实际 %q", v)
	}

	// 非法值回退缺省：置 0 → 上限回落 defaultLoginIPLimitMax（>0），限频仍有效。
	setOpts(t, h, map[string]string{OptionKeyAuthLoginIPLimit: "0"})
	if h.loginIPLimitMax() != defaultLoginIPLimitMax {
		t.Fatalf("非法值应回退缺省 %d，实际 %d", defaultLoginIPLimitMax, h.loginIPLimitMax())
	}
}

// TestLoginTwoFactorBudget M-B ②：2FA 用户完整登录（login + 2fa 两段成功响应
// 均不记账）循环 6 轮 > 上限 5 全部成功；错码失败才记账，耗尽后 login 预检同样
// 429（两段共用 login|IP 失败预算）。
func TestLoginTwoFactorBudget(t *testing.T) {
	r, st, h := newTestAPI(t)
	setOpts(t, h, map[string]string{OptionKeyAuthLoginIPLimit: "5"})
	seedUser(t, st, "carol", "carol-pass", model.RoleUser)
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "carol", "password": "carol-pass"})
	cookie := sessionCookieFrom(t, w)
	secret := enableTOTPFor(t, r, cookie)

	// 完整登录 6 轮：若回到「放行即记账」，一次登录耗 2 份，第 3 轮即 429。
	for i := 0; i < 6; i++ {
		w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
			map[string]string{"username": "carol", "password": "carol-pass"})
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 轮第一段应 200（成功零消耗），实际 %d body=%s", i+1, w.Code, w.Body.String())
		}
		stage1 := sessionCookieFrom(t, w)
		w = doJSON(t, r, http.MethodPost, "/api/user/login/2fa", stage1,
			map[string]string{"code": mustCode(t, secret)})
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 轮第二段应 200，实际 %d body=%s", i+1, w.Code, w.Body.String())
		}
	}

	// 错码失败记账：5 次错码（每轮先拿新 stage1，登录 200 不记账）耗尽预算。
	for i := 0; i < 5; i++ {
		w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
			map[string]string{"username": "carol", "password": "carol-pass"})
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 轮取 stage1 应 200，实际 %d", i+1, w.Code)
		}
		stage1 := sessionCookieFrom(t, w)
		w = doJSON(t, r, http.MethodPost, "/api/user/login/2fa", stage1,
			map[string]string{"code": "000000"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次错码应 400，实际 %d", i+1, w.Code)
		}
	}
	// 预算耗尽：login 第一段也被预检拒绝（共用失败预算，爆破防护保持）。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "carol", "password": "carol-pass"})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("失败预算耗尽后登录应 429，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestSendVerificationCodeIPLimit M-C：sendcode|IP 独立预算 1h×10（放行即记账
// ——每次成功发码都有真实出站成本），超限 429 + Retry-After；与邮箱维度 60s
// 限频正交（每请求换邮箱隔离 vstore 限频）。
func TestSendVerificationCodeIPLimit(t *testing.T) {
	r, _, h := newTestAPI(t)
	setOpts(t, h, map[string]string{mailer.KeyEnabled: "true"})
	h.mailer = &fakeMailer{}

	for i := 0; i < sendcodeIPLimitMax; i++ {
		w := doJSON(t, r, http.MethodPost, "/api/verification_code", "",
			map[string]string{"email": fmt.Sprintf("u%02d@example.com", i), "purpose": "register"})
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次发码应 200，实际 %d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	w := doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "over@example.com", "purpose": "register"})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("超限发码应 429 rate_limited，实际 %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应携带 Retry-After 头")
	}
}

// TestSendVerificationCodeTurnstile M-C：Turnstile 启用时发码与注册同规则强制
// （mock 拒绝 → 400 turnstile_failed 且 verifier 被调用；mock 放行 → 200）。
func TestSendVerificationCodeTurnstile(t *testing.T) {
	r, _, h := newTestAPI(t)
	setOpts(t, h, map[string]string{
		mailer.KeyEnabled:           "true",
		OptionKeyTurnstileEnabled:   "true",
		OptionKeyTurnstileSiteKey:   "site-x",
		OptionKeyTurnstileSecretKey: "secret-x",
	})
	ft := &fakeTurnstile{ok: false}
	h.verifier = ft
	h.mailer = &fakeMailer{}

	w := doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "tt@example.com", "purpose": "register"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "turnstile_failed") {
		t.Fatalf("人机校验拒绝应 400 turnstile_failed，实际 %d body=%s", w.Code, w.Body.String())
	}
	if ft.calls != 1 {
		t.Fatalf("verifier 应被调用 1 次，实际 %d", ft.calls)
	}

	ft.ok = true
	w = doJSON(t, r, http.MethodPost, "/api/verification_code", "",
		map[string]string{"email": "tt2@example.com", "purpose": "register", "turnstile_token": "tok"})
	if w.Code != http.StatusOK {
		t.Fatalf("人机校验通过应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestTOTPFailLimit M-D：enable/disable 共享 totp|<uid> 失败预算——错码记账、
// 正确码不受预算内历史失败影响放行；连续失败达上限后 429（正确码也被拦）。
func TestTOTPFailLimit(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "bob", "bob-pass", model.RoleUser)
	cookie := loginAs(t, r, "bob", "bob-pass")
	secret := enableTOTPFor(t, r, cookie)

	// 预算内错码不拦正确码：错码 3 次 → 400（记账），正确码 disable → 200。
	for i := 0; i < 3; i++ {
		w := doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
			map[string]string{"code": "000000"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次错码应 400，实际 %d", i+1, w.Code)
		}
	}
	w := doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
		map[string]string{"code": mustCode(t, secret)})
	if w.Code != http.StatusOK {
		t.Fatalf("正确码应不受预算内历史失败影响放行（200），实际 %d body=%s", w.Code, w.Body.String())
	}

	// 重新启用后连续失败超限：错码 totpFailLimit 次 → 第 totpFailLimit+1 次 429。
	secret2 := enableTOTPFor(t, r, cookie)
	for i := 0; i < totpFailLimit; i++ {
		w = doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
			map[string]string{"code": "000000"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("超限前第 %d 次错码应 400，实际 %d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/totp/disable", cookie,
		map[string]string{"code": mustCode(t, secret2)})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("失败超限后（即使正确码）应 429，实际 %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应携带 Retry-After 头")
	}
}
