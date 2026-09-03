// handler.go 是管理面 API 的路由注册与登录/登出端点（M2-wave1，docs/05）。
// 管理 CRUD（渠道/令牌/用户/兑换码/日志/配置）在 chunk 中按域拆分文件挂载。
// M3-wave1 新增公开注册体系路由（register.go）与登录 IP 限频。
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
}

// New 构造管理面处理器（含 M3-wave1 注册体系默认依赖）。
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
	}
}

// StartVerificationSweeper 启动验证码过期清扫后台任务（返回停止函数；
// main 停机时调用释放资源）。
func (h *Handler) StartVerificationSweeper(interval time.Duration) (stop func()) {
	return h.vstore.StartSweeper(interval)
}

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
	// 登录用户自服务端点（M2-wave3）：仅要求登录，不要求 root（docs/05）。
	g.POST("/user/topup", h.RequireAuth, h.TopupRedeem)
	g.GET("/user/self", h.RequireAuth, h.GetSelf)
	// 自服务统计（M2 收官）：普通用户看板数据源，服务端按会话用户聚合今日 logs。
	g.GET("/user/stats", h.RequireAuth, h.GetUserStats)
	g.POST("/token/:id/assign", h.RequireAuth, h.AssignTokenQuota)
	// 名下令牌列表（M2 缺陷修复）：登录态 + 所有权作用域。
	g.GET("/token/mine", h.RequireAuth, h.ListMyTokens)
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

// writeErr 写出统一错误响应。
func writeErr(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "message": msg, "code": code})
}

// writeOK 写出统一成功响应。
func writeOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": data})
}
