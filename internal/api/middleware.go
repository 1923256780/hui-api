// middleware.go 是管理面鉴权中间件（M2-wave1）：
//
//   - RequireAuth：会话有效 + 用户存在且启用 + auth_version 匹配（旧会话失效）；
//   - RequireRoot：RequireAuth 语义 + role=100（管理 CRUD 全量要求）。
//
// 两者均为独立中间件（RequireRoot 内部完成登录校验，不与 RequireAuth 串联叠加）。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// ctxUserKey 是 gin.Context 中注入当前用户的键。
const ctxUserKey = "api.authedUser"

// authedUser 是通过鉴权后注入上下文的用户信息。
type authedUser struct {
	ID       int64
	Username string
	Role     int
}

// RequireAuth 校验会话并加载用户：无效会话/用户不存在/被禁用/auth_version
// 不匹配（改密后旧会话失效）一律 401。通过后注入 currentUser。
func (h *Handler) RequireAuth(c *gin.Context) {
	uid, authv, ok := h.sess.Verify(c)
	if !ok {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效或已过期")
		c.Abort()
		return
	}
	var u model.User
	if err := h.st.Read.First(&u, uid).Error; err != nil ||
		u.Status != model.StatusEnabled || u.AuthVersion != authv {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效或已过期")
		c.Abort()
		return
	}
	c.Set(ctxUserKey, authedUser{ID: u.ID, Username: u.Username, Role: u.Role})
}

// RequireRoot 在 RequireAuth 语义之上要求 root（role=100）；普通用户 403。
func (h *Handler) RequireRoot(c *gin.Context) {
	h.RequireAuth(c)
	if c.IsAborted() {
		return
	}
	if u := currentUser(c); u == nil || u.Role != model.RoleAdmin {
		writeErr(c, http.StatusForbidden, "forbidden", "需要管理员权限")
		c.Abort()
	}
}

// currentUser 读取中间件注入的当前用户；未鉴权返回 nil。
func currentUser(c *gin.Context) *authedUser {
	v, ok := c.Get(ctxUserKey)
	if !ok {
		return nil
	}
	if u, ok := v.(authedUser); ok {
		return &u
	}
	return nil
}
