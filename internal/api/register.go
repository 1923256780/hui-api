// register.go 公开注册体系端点（M3-wave1，docs/05）：
//
//   - GET  /api/setup：注册能力发现（注册/邮箱验证/人机校验/OAuth 可用性）；
//   - POST /api/user/register：开放注册（开关、IP 限频、人机校验、邮箱验证码、
//     查重、bcrypt、事务建户与邀请奖励双向入账）；
//   - POST /api/verification_code：发送邮箱验证码（SMTP 配置门控 + 限频）；
//   - POST /api/user/reset_password：验证码重置口令（验码一次性消费 + auth_version 递增）。
//
// 鉴权层级：四个端点全部公开（无需会话），安全边界由开关、限频与验证码承担。
// IP 限频复用 internal/ratelimit 滑动窗口（authRL 与转发链路限流器隔离，
// 避免注册/登录拒绝影响转发配额语义）；登录 IP 限频见 handler.go Login。
// M3-wave2：OAuth 登录/绑定与 TOTP/个人中心端点拆分至 oauth.go/totp.go/profile.go。
package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/mailer"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/verification"
)

// 运行轨配置键（options 白名单 register.*/aff.* 前缀，docs/05 键表）。
const (
	OptionKeyRegisterEnabled           = "register.enabled"
	OptionKeyRegisterEmailVerification = "register.email_verification"
	OptionKeyRegisterQuotaForNewUser   = "register.quota_for_new_user"
	OptionKeyAffRewardInviter          = "aff.register_reward_inviter"
	OptionKeyAffRewardInvitee          = "aff.register_reward_invitee"
)

// 注册/找回的验证码 purpose 枚举（verification.Store 维度隔离键）。
const (
	purposeRegister = "register"
	purposeReset    = "reset"
)

// IP 限频参数（M3-wave1 固定值，不入 options）。
const (
	registerIPLimitWindow = time.Hour
	registerIPLimitMax    = 5
	loginIPLimitWindow    = time.Hour
	loginIPLimitMax       = 10
)

// FeatureFlags 返回公开特性开关（GET /api/setup 与 /api/status features 块共用）。
// OAuth 三项按 options 配置真实探测（M3-wave2：github/linuxdo/oidc 各自
// client_id/secret 配齐才可用，oidc 另需 issuer）。
func FeatureFlags(rt *config.Runtime) gin.H {
	registerEnabled := rt.GetBool(OptionKeyRegisterEnabled, false)
	emailVerification := rt.GetBool(OptionKeyRegisterEmailVerification, false)
	turnstileSiteKey := ""
	if rt.GetBool(OptionKeyTurnstileEnabled, false) {
		turnstileSiteKey, _ = rt.Get(OptionKeyTurnstileSiteKey)
	}
	return gin.H{
		"register_enabled":   registerEnabled,
		"email_verification": emailVerification,
		"turnstile_site_key": turnstileSiteKey,
		"oauth": gin.H{
			"github":  oauthProviderConfigured(rt, ProviderGitHub),
			"linuxdo": oauthProviderConfigured(rt, ProviderLinuxDO),
			"oidc":    oauthProviderConfigured(rt, ProviderOIDC),
		},
		// 在线充值网关开关（M3-wave3，docs/05 §5.10）：前端据此渲染充值区。
		"topup": gin.H{
			"epay":   rt.GetBool(OptionKeyEpayEnabled, false),
			"stripe": rt.GetBool(OptionKeyStripeEnabled, false),
		},
	}
}

// GetSetup 注册能力发现（公开）：前端注册页据此渲染表单与校验项。
func (h *Handler) GetSetup(c *gin.Context) {
	writeOK(c, FeatureFlags(h.rt))
}

// RegisterUser 开放注册。流程（任一步失败即拒绝，无副作用落库）：
// 开关门控 → IP 限频 → 参数校验 → 人机校验 → 邮箱验证码 → 查重 →
// 事务（建户 + 邀请奖励双向入账 + aff 日志）。
func (h *Handler) RegisterUser(c *gin.Context) {
	if !h.rt.GetBool(OptionKeyRegisterEnabled, false) {
		writeErr(c, http.StatusForbidden, "register_disabled", "注册未开放")
		return
	}
	if ok, retry := h.authRL.AllowRequest("register|"+c.ClientIP(), registerIPLimitWindow, registerIPLimitMax, 0); !ok {
		writeRetryAfter(c, retry)
		return
	}
	var req struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		Email          string `json:"email"`
		Code           string `json:"code"`
		AffCode        string `json:"aff_code"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	username := strings.TrimSpace(req.Username)
	password := req.Password
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if username == "" || password == "" || email == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "username、password、email 必填")
		return
	}
	if !strings.Contains(email, "@") {
		writeErr(c, http.StatusBadRequest, "invalid_request", "email 格式不正确")
		return
	}
	if len(password) < 6 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "password 至少 6 位")
		return
	}
	// 人机校验：开启即强制（siteverify 失败按拒绝处理，不阻断可用性由关闭开关承担）。
	if h.rt.GetBool(OptionKeyTurnstileEnabled, false) {
		secret, _ := h.rt.Get(OptionKeyTurnstileSecretKey)
		ok, err := h.verifier.Verify(c.Request.Context(), secret, req.TurnstileToken, c.ClientIP())
		if err != nil || !ok {
			writeErr(c, http.StatusBadRequest, "turnstile_failed", "人机校验未通过")
			return
		}
	}
	// 邮箱验证码：开启即强制；码一次性消费（失败不落库，注册失败可重发）。
	if h.rt.GetBool(OptionKeyRegisterEmailVerification, false) {
		if h.vstore == nil {
			writeErr(c, http.StatusInternalServerError, "verification_unavailable", "验证码服务不可用")
			return
		}
		if err := h.vstore.Verify(email, purposeRegister, req.Code); err != nil {
			writeVerificationErr(c, err)
			return
		}
	}
	// 查重：username 与 email 冲突均 409（注册场景用户需要明确归因，非登录语义）。
	var n int64
	if err := h.st.Read.Model(&model.User{}).Where("username = ?", username).Count(&n).Error; err == nil && n > 0 {
		writeErr(c, http.StatusConflict, "username_conflict", "用户名已存在")
		return
	}
	if err := h.st.Read.Model(&model.User{}).Where("email = ?", email).Count(&n).Error; err == nil && n > 0 {
		writeErr(c, http.StatusConflict, "email_conflict", "邮箱已被注册")
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "register_failed", "口令处理失败")
		return
	}
	// 邀请人解析：aff_code 无效/自引用不阻断注册（docs/05 注册矩阵）。
	affCode := strings.TrimSpace(req.AffCode)
	var inviter model.User
	hasInviter := false
	if affCode != "" {
		if err := h.st.Read.Where("aff_code = ?", affCode).First(&inviter).Error; err == nil && inviter.ID != 0 {
			hasInviter = true
		}
	}
	newQuota := h.rt.GetInt64(OptionKeyRegisterQuotaForNewUser, 0)
	now := time.Now().Unix()
	u := model.User{
		Username:     username,
		PasswordHash: hash,
		DisplayName:  username,
		Role:         model.RoleUser,
		Status:       model.StatusEnabled,
		Quota:        newQuota,
		Email:        email,
		Group:        "default",
		AuthVersion:  1,
		AffCode:      generateAffCode(),
		CreatedTime:  now,
	}
	if hasInviter {
		u.InviterID = inviter.ID
	}
	created := int64(0)
	err = h.st.Write.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		created = u.ID
		if !hasInviter {
			return nil
		}
		// 邀请奖励双向入账（事务内：建户与奖励原子；SQLite 单写池串行化 +
		// gorm.Expr 原地累加保证并发注册时邀请人收益恰为 N×reward）。
		inviterReward := h.rt.GetInt64(OptionKeyAffRewardInviter, 0)
		inviteeReward := h.rt.GetInt64(OptionKeyAffRewardInvitee, 0)
		if inviterReward > 0 {
			res := tx.Model(&model.User{}).Where("id = ?", inviter.ID).
				Updates(map[string]any{
					"quota":             gorm.Expr("quota + ?", inviterReward),
					"aff_history_quota": gorm.Expr("aff_history_quota + ?", inviterReward),
				})
			if res.Error != nil {
				return res.Error
			}
			if err := tx.Create(&model.Log{
				UserID:      inviter.ID,
				Protocol:    "aff",
				ModelName:   "register_reward_inviter",
				Quota:       inviterReward,
				Detail:      affRewardDetail(u.ID, inviterReward),
				CreatedTime: now,
			}).Error; err != nil {
				return err
			}
		}
		if inviteeReward > 0 {
			res := tx.Model(&model.User{}).Where("id = ?", u.ID).
				Update("quota", gorm.Expr("quota + ?", inviteeReward))
			if res.Error != nil {
				return res.Error
			}
			if err := tx.Create(&model.Log{
				UserID:      u.ID,
				Protocol:    "aff",
				ModelName:   "register_reward_invitee",
				Quota:       inviteeReward,
				Detail:      affRewardDetail(inviter.ID, inviteeReward),
				CreatedTime: now,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "register_failed", "注册失败")
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"success": true, "message": "",
		"data": gin.H{"id": created, "username": u.Username, "aff_code": u.AffCode},
	})
}

// SendVerificationCode 发送邮箱验证码（公开）。SMTP 未启用 503；同邮箱
// 60s 限频 / 每日上限由 verification.Store 承担（429）。验证码不回显不落日志。
func (h *Handler) SendVerificationCode(c *gin.Context) {
	var req struct {
		Email   string `json:"email"`
		Purpose string `json:"purpose"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	purpose := strings.TrimSpace(req.Purpose)
	if email == "" || !strings.Contains(email, "@") {
		writeErr(c, http.StatusBadRequest, "invalid_request", "email 格式不正确")
		return
	}
	if purpose != purposeRegister && purpose != purposeReset {
		writeErr(c, http.StatusBadRequest, "invalid_request", "purpose 仅支持 register / reset")
		return
	}
	if !h.rt.GetBool(mailer.KeyEnabled, false) {
		writeErr(c, http.StatusServiceUnavailable, "smtp_not_configured", "邮件服务未配置")
		return
	}
	if h.vstore == nil {
		writeErr(c, http.StatusInternalServerError, "verification_unavailable", "验证码服务不可用")
		return
	}
	code, err := h.vstore.Issue(email, purpose)
	if err != nil {
		if errors.Is(err, verification.ErrTooFrequent) || errors.Is(err, verification.ErrDailyLimit) {
			writeErr(c, http.StatusTooManyRequests, "rate_limited", "发送过于频繁，请稍后再试")
			return
		}
		writeErr(c, http.StatusInternalServerError, "verification_issue_failed", "生成验证码失败")
		return
	}
	if h.mailer == nil {
		writeErr(c, http.StatusServiceUnavailable, "smtp_not_configured", "邮件服务未配置")
		return
	}
	subject := "Hui Api 验证码"
	if purpose == purposeReset {
		subject = "Hui Api 密码重置验证码"
	}
	body := "您的验证码是 " + code + "，10 分钟内有效。若非本人操作请忽略本邮件。"
	if err := h.mailer.Send(email, subject, body); err != nil {
		writeErr(c, http.StatusInternalServerError, "mail_send_failed", "邮件发送失败，请稍后再试")
		return
	}
	writeOK(c, gin.H{"sent": true})
}

// ResetPassword 验证码重置口令（公开）：验码（一次性消费）→ bcrypt 新口令 →
// auth_version 递增（既有会话全部失效）。目标邮箱不存在返回 404（码已消费，
// 枚举探测需先拿到有效码，泄露面可控）。
func (h *Handler) ResetPassword(c *gin.Context) {
	if ok, retry := h.authRL.AllowRequest("reset|"+c.ClientIP(), registerIPLimitWindow, registerIPLimitMax, 0); !ok {
		writeRetryAfter(c, retry)
		return
	}
	var req struct {
		Email       string `json:"email"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	newPassword := req.NewPassword
	if email == "" || req.Code == "" || newPassword == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "email、code、new_password 必填")
		return
	}
	if len(newPassword) < 6 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "new_password 至少 6 位")
		return
	}
	if h.vstore == nil {
		writeErr(c, http.StatusInternalServerError, "verification_unavailable", "验证码服务不可用")
		return
	}
	if err := h.vstore.Verify(email, purposeReset, req.Code); err != nil {
		writeVerificationErr(c, err)
		return
	}
	var u model.User
	if err := h.st.Read.Where("email = ?", email).First(&u).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "邮箱未绑定任何账号")
		return
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "reset_failed", "口令处理失败")
		return
	}
	if err := h.st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Updates(map[string]any{"password_hash": hash, "auth_version": u.AuthVersion + 1}).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "reset_failed", "重置口令失败")
		return
	}
	writeOK(c, gin.H{"reset": true})
}

// writeVerificationErr 把验证码校验错误映射为 400 响应（区分失效与不匹配，
// 便于前端提示；均为 400 非枚举面——码本身不泄露存在性）。
func writeVerificationErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, verification.ErrNotFound):
		writeErr(c, http.StatusBadRequest, "code_invalid_or_expired", "验证码无效或已过期")
	case errors.Is(err, verification.ErrMismatch):
		writeErr(c, http.StatusBadRequest, "code_mismatch", "验证码不正确")
	default:
		writeErr(c, http.StatusBadRequest, "verification_failed", "验证码校验失败")
	}
}

// writeRetryAfter 写出 429 响应（带 Retry-After 秒数，向上取整）。
func writeRetryAfter(c *gin.Context, retry time.Duration) {
	secs := int(retry/time.Second) + 1
	c.Header("Retry-After", strconv.Itoa(secs))
	writeErr(c, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
}

// affRewardDetail 构造 aff 类日志 detail JSON（仅事件与对象 ID，无敏感信息）。
func affRewardDetail(otherUserID, quota int64) string {
	b, err := json.Marshal(map[string]any{
		"event":  "register_reward",
		"ref_id": otherUserID,
		"quota":  quota,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// affCodeChars 是邀请码字符集（去易混淆字符 0/O/1/I/l）。
const affCodeChars = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// generateAffCode 生成 8 位随机邀请码（crypto/rand；碰撞域 31^8，冲突概率
// 可忽略且无唯一约束依赖——注册时若撞码仅影响按码查邀请人，接受该风险）。
func generateAffCode() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	for i := range buf {
		buf[i] = affCodeChars[int(buf[i])%len(affCodeChars)]
	}
	return string(buf)
}
