// redemption.go 兑换码管理端点（M2-wave1，docs/05）：root 权限批量生成/列表/删除。
// 明文 key 仅在生成响应返回一次；核销逻辑（兑换→令牌加额）属 wave3 状态机。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// registerRedemptionRoutes 挂载 /api/redemption 路由。
func (h *Handler) registerRedemptionRoutes(g *gin.RouterGroup) {
	g.GET("/redemption", h.ListRedemptions)
	g.POST("/redemption", h.CreateRedemptions)
	g.DELETE("/redemption/:id", h.DeleteRedemption)
}

// redemptionRequest 是批量生成请求：count 1..100，quota 必填（与令牌同单位），
// expired_time 0 表示永久有效。
type redemptionRequest struct {
	Count       int    `json:"count"`
	Name        string `json:"name"`
	Quota       int64  `json:"quota"`
	ExpiredTime int64  `json:"expired_time"`
}

// redemptionKeyMaxRetry 生成 key 的冲突重试上限（uniqueIndex 冲突极低，防御式）。
const redemptionKeyMaxRetry = 3

// generateRedemptionKey 生成明文兑换码："redd-" + 24 位随机 hex（96 bit 熵）。
func generateRedemptionKey() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "redd-" + hex.EncodeToString(b), nil
}

// ListRedemptions 分页列表（id desc）。
func (h *Handler) ListRedemptions(c *gin.Context) {
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.Redemption{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询兑换码失败")
		return
	}
	var rows []model.Redemption
	if err := q.Order("id desc").Offset((page - 1) * pageSize).
		Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询兑换码失败")
		return
	}
	writeOK(c, gin.H{"items": rows, "total": total, "page": page, "page_size": pageSize})
}

// CreateRedemptions 批量生成：key 冲突重试后仍失败则整体拒绝（不留半批）。
// CreatedBy 记录操作者（当前登录 root）。
func (h *Handler) CreateRedemptions(c *gin.Context) {
	var req redemptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Count < 1 || req.Count > 100 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "count 取值 1..100")
		return
	}
	if req.Quota <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "quota 必须大于 0")
		return
	}
	createdBy := int64(0)
	if u := currentUser(c); u != nil {
		createdBy = u.ID
	}
	expired := req.ExpiredTime
	if expired == 0 {
		expired = model.EpochForever
	}
	now := time.Now().Unix()
	rows := make([]model.Redemption, 0, req.Count)
	keys := make([]string, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		key, err := generateRedemptionKey()
		if err != nil {
			writeErr(c, http.StatusInternalServerError, "create_failed", "生成兑换码失败")
			return
		}
		row := model.Redemption{
			Key: key, Name: req.Name, Status: model.StatusEnabled,
			Quota: req.Quota, CreatedBy: createdBy,
			ExpiredTime: expired, CreatedTime: now,
		}
		ok := false
		for retry := 0; retry < redemptionKeyMaxRetry; retry++ {
			if err := h.st.Write.Create(&row).Error; err != nil {
				// key 唯一索引冲突：换一个再试。
				if key, kerr := generateRedemptionKey(); kerr == nil {
					row.Key = key
					continue
				}
				writeErr(c, http.StatusInternalServerError, "create_failed", "生成兑换码失败")
				return
			}
			ok = true
			break
		}
		if !ok {
			writeErr(c, http.StatusInternalServerError, "create_failed", "兑换码 key 冲突，请重试")
			return
		}
		rows = append(rows, row)
		keys = append(keys, row.Key)
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "message": "",
		"data": gin.H{"items": rows, "keys": keys}})
}

// DeleteRedemption 删除单个兑换码（已核销的记录同样可删，审计走 logs 表）。
func (h *Handler) DeleteRedemption(c *gin.Context) {
	var row model.Redemption
	if err := h.st.Read.First(&row, paramID(c)).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "兑换码不存在")
		return
	}
	if err := h.st.Write.Delete(&row).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "删除兑换码失败")
		return
	}
	writeOK(c, nil)
}
