// profile.go 个人中心自服务端点（M3-wave2，docs/05 §5.9）：
//
//   - POST /api/user/password：修改口令（登录态；旧口令校验，OAuth 建户的
//     无口令账号首次设置免验旧口令）；成功后 auth_version 递增并重签完整
//     会话——全部旧会话（其他设备）失效，当前会话无缝续用；
//   - POST /api/user/email：修改邮箱（登录态；格式校验 + 查重）。
//
// 邮箱修改暂不强制邮箱验证码（后续商业化增强项，注记 docs/05）；修改邮箱
// 后即可走忘记密码流程找回无口令账号。
package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// ChangeMyPassword 修改本人口令（登录态）：
//   - 既有口令用户：old_password 必须匹配（400 old_password_mismatch）；
//   - OAuth 建户无口令账号：首次设置口令免验旧口令（会话本身即授权依据）；
//   - 新口令 ≥6 位；成功后 auth_version++（其他会话全部失效）并重签
//     Stage=0 完整会话（当前会话续用，无需重新登录）。
func (h *Handler) ChangeMyPassword(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(req.NewPassword) < 6 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "new_password 至少 6 位")
		return
	}
	var row model.User
	if err := h.st.Read.First(&row, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if row.PasswordHash != "" && !CheckPassword(row.PasswordHash, req.OldPassword) {
		writeErr(c, http.StatusBadRequest, "old_password_mismatch", "旧口令不正确")
		return
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "update_failed", "口令处理失败")
		return
	}
	if err := h.st.Write.Model(&model.User{}).Where("id = ?", row.ID).
		Updates(map[string]any{"password_hash": hash, "auth_version": row.AuthVersion + 1}).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "update_failed", "修改口令失败")
		return
	}
	// 重签完整会话：旧 cookie 因 auth_version 变化失效，新 cookie 即时下发。
	h.sess.Issue(c, row.ID, row.AuthVersion+1)
	writeOK(c, nil)
}

// ChangeMyEmail 修改本人邮箱（登录态）：格式校验（含 @）+ 查重
// （409 email_conflict）；邮箱用于忘记密码与（未来的）注册验证码场景。
func (h *Handler) ChangeMyEmail(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeErr(c, http.StatusBadRequest, "invalid_request", "email 格式不正确")
		return
	}
	var row model.User
	if err := h.st.Read.First(&row, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if email != row.Email {
		var n int64
		if err := h.st.Read.Model(&model.User{}).Where("email = ?", email).Count(&n).Error; err != nil {
			writeErr(c, http.StatusInternalServerError, "query_failed", "查询邮箱失败")
			return
		}
		if n > 0 {
			writeErr(c, http.StatusConflict, "email_conflict", "邮箱已被其他账号使用")
			return
		}
	}
	if err := h.st.Write.Model(&model.User{}).Where("id = ?", row.ID).
		Update("email", email).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "update_failed", "修改邮箱失败")
		return
	}
	writeOK(c, gin.H{"email": email})
}
