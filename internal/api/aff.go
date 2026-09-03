// aff.go 邀请信息自服务端点（M3-wave3，docs/05）：
//
//   - GET /api/user/aff：当前用户邀请码 / 邀请人数 / 累计返利 / 返利比例。
//     aff_code 惰性补发（wave1 前注册的老用户或异常空码在首次访问时生成落库）。
//     返利入账发生在充值结算事务内（order.go settleTopupOrder），此处只读。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// GetMyAff 返回当前登录用户的邀请信息（登录态，本人作用域）。
func (h *Handler) GetMyAff(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var row model.User
	if err := h.st.Read.First(&row, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	code := row.AffCode
	if code == "" {
		// 惰性补发邀请码：碰撞域 31^8，冲突概率可忽略（同 generateAffCode 注记）。
		code = generateAffCode()
		if err := h.st.Write.Model(&model.User{}).Where("id = ?", row.ID).
			Update("aff_code", code).Error; err != nil {
			writeErr(c, http.StatusInternalServerError, "aff_failed", "邀请码生成失败")
			return
		}
	}
	var invited int64
	if err := h.st.Read.Model(&model.User{}).Where("inviter_id = ?", row.ID).
		Count(&invited).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "aff_failed", "邀请信息查询失败")
		return
	}
	writeOK(c, gin.H{
		"aff_code":          code,
		"invited_count":     invited,
		"aff_history_quota": row.AffHistoryQuota,
		"rebate_percent":    h.rt.GetInt64(OptionKeyAffRebatePercent, 0),
	})
}
