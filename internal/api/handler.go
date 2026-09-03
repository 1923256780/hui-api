// handler.go 是管理面 API 的路由注册与登录/登出端点（M2-wave1，docs/05）。
// 管理 CRUD（渠道/令牌/用户/兑换码/日志/配置）在 chunk 中按域拆分文件挂载。
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// Handler 是管理面 API 处理器集合（按域拆分文件，共享 rt/gw/sess）。
type Handler struct {
	st   *store.Store
	rt   *config.Runtime
	gw   *gateway.Gateway // 渠道/令牌写后失效挂接（Reset/Invalidate）；测试可传 nil
	sess *SessionManager
}

// New 构造管理面处理器。
func New(st *store.Store, rt *config.Runtime, gw *gateway.Gateway, sess *SessionManager) *Handler {
	return &Handler{st: st, rt: rt, gw: gw, sess: sess}
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

// Register 挂载 /api 路由组：login/logout 公开；其余管理端点 root 权限，
// 由各域文件在 registerManaged 中挂载。
func (h *Handler) Register(r gin.IRouter) {
	g := r.Group("/api")
	g.POST("/user/login", h.Login)
	g.POST("/user/logout", h.Logout)
	h.registerManaged(g.Group("", h.RequireRoot))
}

// registerManaged 挂载 root 权限的管理端点（各域文件分段注册）。
func (h *Handler) registerManaged(g *gin.RouterGroup) {
	h.registerChannelRoutes(g)
	h.registerOptionRoutes(g)
}

// Login 用户名密码登录：校验 users 表（bcrypt），通过后签发签名会话 cookie。
// 用户名不存在与密码错误返回同一错误（不泄露账号存在性）。
func (h *Handler) Login(c *gin.Context) {
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
