// user.go 用户管理端点（M2-wave1，docs/05）：root 管理普通用户。
// 修改口令时递增 users.auth_version 使该用户全部既有会话失效；
// 删除用户级联删除其全部令牌并失效鉴权缓存；禁止删除/降级管理员自身
// 所在的 root 账号链路造成锁死（root 不可删除、不可自改 role/status）。
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// registerUserRoutes 挂载 /api/user 路由。
func (h *Handler) registerUserRoutes(g *gin.RouterGroup) {
	g.GET("/user", h.ListUsers)
	g.POST("/user", h.CreateUser)
	g.PUT("/user/:id", h.UpdateUser)
	g.DELETE("/user/:id", h.DeleteUser)
}

// userRequest 是用户创建/更新的完整请求对象（整对象写）。
// Password 创建必填；更新为空 = 不修改口令。
type userRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        int    `json:"role"`
	Status      int    `json:"status"`
	Quota       int64  `json:"quota"`
	Group       string `json:"group"`
	Email       string `json:"email"`
}

// ListUsers 分页列表（PasswordHash/AuthVersion 序列化豁免）。
func (h *Handler) ListUsers(c *gin.Context) {
	page, pageSize := pagination(c)
	var total int64
	if err := h.st.Read.Model(&model.User{}).Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询用户失败")
		return
	}
	var rows []model.User
	if err := h.st.Read.Order("id asc").Offset((page - 1) * pageSize).
		Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询用户失败")
		return
	}
	writeOK(c, gin.H{"items": rows, "total": total, "page": page, "page_size": pageSize})
}

// CreateUser 创建用户：用户名唯一、口令必填（bcrypt）；缺省普通用户/启用/
// 分组 default。
func (h *Handler) CreateUser(c *gin.Context) {
	var req userRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "username 与 password 必填")
		return
	}
	var n int64
	if err := h.st.Read.Model(&model.User{}).Where("username = ?", username).Count(&n).Error; err != nil || n > 0 {
		writeErr(c, http.StatusConflict, "username_conflict", "用户名已存在")
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "create_failed", "口令处理失败")
		return
	}
	role := req.Role
	if role != model.RoleAdmin {
		role = model.RoleUser
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "default"
	}
	u := model.User{
		Username: username, PasswordHash: hash, DisplayName: req.DisplayName,
		Role: role, Status: model.StatusEnabled,
		Quota: req.Quota, Group: group, Email: req.Email,
		AuthVersion: 1, CreatedTime: time.Now().Unix(),
	}
	if req.Status != 0 {
		u.Status = req.Status
	}
	if err := h.st.Write.Create(&u).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "create_failed", "创建用户失败")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "message": "", "data": u})
}

// UpdateUser 整对象幂等替换：root 不可自改 role/status（防自锁）；
// 口令非空时重置并递增 auth_version（既有会话全部失效）。
func (h *Handler) UpdateUser(c *gin.Context) {
	id := paramID(c)
	var req userRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var u model.User
	if err := h.st.Read.First(&u, id).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if cur := currentUser(c); cur != nil && cur.ID == id &&
		(req.Role != u.Role || req.Status != u.Status) {
		writeErr(c, http.StatusBadRequest, "self_lockout", "不允许修改自己的角色或状态")
		return
	}
	if un := strings.TrimSpace(req.Username); un != "" {
		u.Username = un // 空 = 保留旧值（防误清）
	}
	u.DisplayName = req.DisplayName
	u.Role = req.Role
	if u.Role == 0 {
		u.Role = model.RoleUser // 缺省归一化为普通用户
	}
	u.Status = req.Status
	u.Quota = req.Quota
	u.Email = req.Email
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "default"
	}
	u.Group = group
	if u.Status == 0 {
		u.Status = model.StatusEnabled
	}
	if req.Password != "" {
		hash, err := HashPassword(req.Password)
		if err != nil {
			writeErr(c, http.StatusInternalServerError, "update_failed", "口令处理失败")
			return
		}
		u.PasswordHash = hash
		u.AuthVersion++ // 改密即失效全部旧会话
	}
	if err := h.st.Write.Save(&u).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "update_failed", "更新用户失败")
		return
	}
	writeOK(c, u)
}

// DeleteUser 删除用户：不可删除管理员；级联删除其全部令牌并失效鉴权缓存。
func (h *Handler) DeleteUser(c *gin.Context) {
	id := paramID(c)
	var u model.User
	if err := h.st.Read.First(&u, id).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if u.Role == model.RoleAdmin {
		writeErr(c, http.StatusBadRequest, "delete_admin_forbidden", "不允许删除管理员账号")
		return
	}
	var toks []model.Token
	if err := h.st.Read.Where("user_id = ?", id).Find(&toks).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "查询用户令牌失败")
		return
	}
	if err := h.st.Write.Where("user_id = ?", id).Delete(&model.Token{}).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "删除用户令牌失败")
		return
	}
	if err := h.st.Write.Delete(&u).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "删除用户失败")
		return
	}
	for _, tk := range toks {
		h.invalidateToken(tk.KeyHash)
	}
	writeOK(c, nil)
}
