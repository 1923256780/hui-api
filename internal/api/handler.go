// handler.go 是管理面 API 的路由注册与登录/登出端点（M2-wave1，docs/05）。
// 管理 CRUD（渠道/令牌/用户/兑换码/日志/配置）在 chunk 中按域拆分文件挂载。
// M3-wave1 新增公开注册体系路由（register.go）与登录 IP 限频；M3-wave2 新增
// 二段式登录（Login + /api/user/login/2fa）与 OAuth/TOTP/个人中心路由
// （oauth.go/totp.go/profile.go）；M3-wave3 新增在线充值订单/支付回调/邀请
// 信息路由（order.go/aff.go，docs/05 §5.10）；M3-wave4 新增本人计费日志
// （log.go 的 /api/log/mine）并把验证码清扫收编 worker 池。
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/mailer"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/ratelimit"
	"github.com/1923256780/hui-api/internal/store"
	"github.com/1923256780/hui-api/internal/verification"
)

// Handler 是管理面 API 处理器集合（按域拆分文件，共享 rt/gw/sess）。
type Handler struct {
	st   *store.Store
	rt   *config.Runtime
	gw   *gateway.Gateway // 渠道/令牌写后失效挂接（Reset/Invalidate）；测试可传 nil
	sess *SessionManager

	// M3-wave1 注册体系依赖（New 构造默认实现；测试可整体替换字段注入 mock）：
	authRL   *ratelimit.Limiter  // 登录/注册/重置 IP 限频（与转发链路限流器隔离）
	verifier TurnstileVerifier   // 人机校验（siteverify，5s 超时）
	mailer   mailer.Mailer       // SMTP 邮件发送（smtp.enabled 门控）
	vstore   *verification.Store // 邮箱验证码存储（email+purpose 维度）

	// M3-wave2 OAuth 依赖（New 构造默认真实端点；测试可替换字段注入
	// httptest 假 provider）：oauthHTTP 为 token/userinfo/发现请求出站客户端，
	// GitHub 三端点与 LinuxDO issuer 常量默认值仅测试覆盖用。
	oauthHTTP               *http.Client
	oauthGithubAuthorizeURL string
	oauthGithubTokenURL     string
	oauthGithubUserinfoURL  string
	oauthLinuxDOIssuer      string

	// M3-wave3 支付依赖（New 构造默认实现；测试可替换字段注入 httptest
	// 假 Stripe 端点）：stripeHTTP 为创建 Checkout Session 的出站客户端，
	// stripeAPIBase 为空时用 payment.DefaultStripeAPIBase。
	stripeHTTP    *http.Client
	stripeAPIBase string
}

// New 构造管理面处理器（含 M3-wave1 注册体系与 M3-wave2 OAuth 默认依赖）。
func New(st *store.Store, rt *config.Runtime, gw *gateway.Gateway, sess *SessionManager) *Handler {
	return &Handler{
		st:       st,
		rt:       rt,
		gw:       gw,
		sess:     sess,
		authRL:   ratelimit.New(nil),
		verifier: newTurnstileVerifier(),
		mailer:   mailer.New(func(key string) (string, bool) { return rt.Get(key) }),
		vstore:   verification.New(nil),

		oauthHTTP:               &http.Client{Timeout: oauthHTTPTimeout},
		oauthGithubAuthorizeURL: "https://github.com/login/oauth/authorize",
		oauthGithubTokenURL:     "https://github.com/login/oauth/access_token",
		oauthGithubUserinfoURL:  "https://api.github.com/user",
		oauthLinuxDOIssuer:      "https://connect.linux.do",

		stripeHTTP: &http.Client{Timeout: stripeHTTPTimeout},
		// stripeAPIBase 零值为空，CreateTopupOrder 内回退 payment.DefaultStripeAPIBase。
	}
}

// VerificationStore 暴露验证码存储（M3-wave4：main 的 worker 池装配定时清扫，
// Sweep 调用方见 internal/verification；替代原 StartVerificationSweeper）。
func (h *Handler) VerificationStore() *verification.Store { return h.vstore }

// resetChannel 写后使渠道相关运行态失效：复位该渠道熔断计数，新配置立即生效
// （渠道调度无缓存，直接查库）。gw 为 nil 时跳过（最小构造/测试）。
func (h *Handler) resetChannel(id int64) {
	if h.gw != nil {
		h.gw.Breaker().Reset(id)
	}
}

// pagination 解析分页参数：page 缺省 1，page_size 缺省 20 上限 100。
func pagination(c *gin.Context) (page, pageSize int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

// paramID 解析路径参数 :id；非法返回 0。
func paramID(c *gin.Context) int64 {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	return id
}

// Register 挂载 /api 路由组：login/logout 与注册体系（setup/register/
// verification_code/reset_password）公开；自服务端点仅要求登录；其余管理
// 端点 root 权限，由各域文件在 registerManaged 中挂载。
func (h *Handler) Register(r gin.IRouter) {
	g := r.Group("/api")
	g.POST("/user/login", h.Login)
	g.POST("/user/logout", h.Logout)
	// 公开注册体系（M3-wave1，docs/05）。
	g.GET("/setup", h.GetSetup)
	g.POST("/user/register", h.RegisterUser)
	g.POST("/verification_code", h.SendVerificationCode)
	g.POST("/user/reset_password", h.ResetPassword)
	// OAuth 登录（公开组）与两步验证第二段（凭 stage1 会话，M3-wave2）。
	g.GET("/oauth/:provider", h.OAuthAuthorize)
	g.GET("/oauth/:provider/callback", h.OAuthCallback)
	g.POST("/user/login/2fa", h.LoginTwoFactor)
	// OAuth 绑定模式与身份列表/解绑（登录态，M3-wave2，docs/05 §5.8）。
	g.GET("/oauth/:provider/bind", h.RequireAuth, h.OAuthBindAuthorize)
	g.GET("/user/identities", h.RequireAuth, h.ListMyIdentities)
	g.DELETE("/user/identities/:id", h.RequireAuth, h.DeleteMyIdentity)
	// 两步验证 TOTP 三端点（登录态，M3-wave2，docs/05 §5.9）。
	g.POST("/user/totp/setup", h.RequireAuth, h.TOTPSetup)
	g.POST("/user/totp/enable", h.RequireAuth, h.TOTPEnable)
	g.POST("/user/totp/disable", h.RequireAuth, h.TOTPDisable)
	// 个人中心自服务（登录态，M3-wave2）：改密/改邮箱。
	g.POST("/user/password", h.RequireAuth, h.ChangeMyPassword)
	g.POST("/user/email", h.RequireAuth, h.ChangeMyEmail)
	// 登录用户自服务端点（M2-wave3）：仅要求登录，不要求 root（docs/05）。
	g.POST("/user/topup", h.RequireAuth, h.TopupRedeem)
	g.GET("/user/self", h.RequireAuth, h.GetSelf)
	// 自服务统计（M2 收官）：普通用户看板数据源，服务端按会话用户聚合今日 logs。
	g.GET("/user/stats", h.RequireAuth, h.GetUserStats)
	g.POST("/token/:id/assign", h.RequireAuth, h.AssignTokenQuota)
	// 名下令牌列表（M2 缺陷修复）：登录态 + 所有权作用域。
	g.GET("/token/mine", h.RequireAuth, h.ListMyTokens)
	// 本人计费日志（M3-wave4）：登录态 + 会话作用域 + 白名单字段。
	g.GET("/log/mine", h.RequireAuth, h.ListMyLogs)
	// 在线充值与邀请返利（M3-wave3，docs/05 §5.10）：下单/订单列表/邀请信息
	// 登录态；支付网关回调（notify/return/webhook）公开，安全边界靠验签。
	g.POST("/user/topup/order", h.RequireAuth, h.CreateTopupOrder)
	g.GET("/user/topup/orders", h.RequireAuth, h.ListMyTopupOrders)
	g.GET("/user/aff", h.RequireAuth, h.GetMyAff)
	g.GET("/pay/epay/notify", h.EpayNotify)
	g.GET("/pay/epay/return", h.EpayReturn)
	g.POST("/pay/stripe/webhook", h.StripeWebhook)
	h.registerManaged(g.Group("", h.RequireRoot))
}

// registerManaged 挂载 root 权限的管理端点（各域文件分段注册）。
func (h *Handler) registerManaged(g *gin.RouterGroup) {
	h.registerChannelRoutes(g)
	h.registerTokenRoutes(g)
	h.registerUserRoutes(g)
	h.registerRedemptionRoutes(g)
	h.registerLogRoutes(g)
	h.registerOptionRoutes(g)
}

// Login 用户名密码登录：IP 限频（1h×10）→ 校验 users 表（bcrypt），通过后
// 签发签名会话 cookie。用户名不存在与密码错误返回同一错误（不泄露账号存在性）。
// M3-wave2 二段式登录：已启用两步验证的用户签 Stage=1 短 TTL 会话并返回
// {require_2fa:true}，前端凭该会话调 POST /api/user/login/2fa 完成第二段
// （TOTPEnabled 且密钥为空的异常态按未启用处理，避免用户被锁死）。
func (h *Handler) Login(c *gin.Context) {
	if ok, retry := h.authRL.AllowRequest("login|"+c.ClientIP(), loginIPLimitWindow, loginIPLimitMax, 0); !ok {
		writeRetryAfter(c, retry)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" || req.Password == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "缺少用户名或密码")
		return
	}
	var u model.User
	if err := h.st.Read.Where("username = ?", req.Username).First(&u).Error; err != nil {
		writeErr(c, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
		return
	}
	if !CheckPassword(u.PasswordHash, req.Password) {
		writeErr(c, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
		return
	}
	if u.Status != model.StatusEnabled {
		writeErr(c, http.StatusForbidden, "user_disabled", "用户已被禁用")
		return
	}
	// 二段式登录第一段（M3-wave2）：Stage=1 + 5min TTL 会话；不更新
	// last_login_time（第二段完成时再记，保证语义为「完整登录时间」）。
	if u.TOTPEnabled && u.TOTPSecret != "" {
		h.sess.IssueStage(c, u.ID, u.AuthVersion, stageTOTP, totpStageTTL)
		writeOK(c, gin.H{"require_2fa": true})
		return
	}
	h.sess.Issue(c, u.ID, u.AuthVersion)
	_ = h.st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Update("last_login_time", time.Now().Unix()).Error
	writeOK(c, gin.H{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"role":         u.Role,
	})
}

// Logout 登出：清除会话 cookie。
func (h *Handler) Logout(c *gin.Context) {
	h.sess.Clear(c)
	writeOK(c, nil)
}

// LoginTwoFactor 两步验证第二段（公开，M3-wave2）：凭 Stage=1 短 TTL 会话
// 验 TOTP 码，通过后重签 Stage=0 完整会话。IP 限频与密码登录共用 login|IP
// 预算（防绕过密码段直接爆破验证码；密码错误 + 多次验码失败会累积触发 429）。
// 阶段/用户校验：会话必须为 stageTOTP 且 auth_version 与库一致（改密后
// stage1 会话同样失效）。
func (h *Handler) LoginTwoFactor(c *gin.Context) {
	if ok, retry := h.authRL.AllowRequest("login|"+c.ClientIP(), loginIPLimitWindow, loginIPLimitMax, 0); !ok {
		writeRetryAfter(c, retry)
		return
	}
	uid, authv, stage, ok := h.sess.Verify(c)
	if !ok {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效或已过期")
		return
	}
	if stage != stageTOTP {
		writeErr(c, http.StatusBadRequest, "invalid_request", "当前会话无需两步验证")
		return
	}
	var u model.User
	if err := h.st.Read.First(&u, uid).Error; err != nil || u.AuthVersion != authv {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效或已过期")
		return
	}
	if u.Status != model.StatusEnabled {
		writeErr(c, http.StatusForbidden, "user_disabled", "用户已被禁用")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "缺少验证码")
		return
	}
	if u.TOTPSecret == "" || !otpValidateCode(req.Code, u.TOTPSecret) {
		writeErr(c, http.StatusBadRequest, "totp_code_invalid", "验证码不正确")
		return
	}
	h.sess.Issue(c, u.ID, u.AuthVersion)
	_ = h.st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Update("last_login_time", time.Now().Unix()).Error
	writeOK(c, gin.H{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"role":         u.Role,
	})
}

// writeErr 写出统一错误响应。
func writeErr(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "message": msg, "code": code})
}

// writeOK 写出统一成功响应。
func writeOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": data})
}
