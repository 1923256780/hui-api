// token.go 令牌管理端点（M2-wave1，docs/05）：root 权限 CRUD。
// 明文密钥 sk- 前缀、仅在创建响应返回一次，库内唯一鉴权依据是 key_hash
// （SHA-256 hex，与转发面鉴权 gateway.HashKey 同源）。PUT 为整对象幂等写；
// 增删改后失效该令牌的鉴权缓存，权限变化即时生效。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
)

// registerTokenRoutes 挂载 /api/token 路由。
func (h *Handler) registerTokenRoutes(g *gin.RouterGroup) {
	g.GET("/token", h.ListTokens)
	g.POST("/token", h.CreateToken)
	g.PUT("/token/:id", h.UpdateToken)
	g.DELETE("/token/:id", h.DeleteToken)
}

// tokenRequest 是令牌创建/更新的完整请求对象（整对象写；key/key_hash/user_id
// 不可经此修改）。Quota 为预算周期总额度；UnlimitedQuota 时余额字段无意义。
type tokenRequest struct {
	UserID         int64  `json:"user_id"`
	Name           string `json:"name"`
	Status         int    `json:"status"`
	Quota          int64  `json:"quota"`
	RemainQuota    int64  `json:"remain_quota"`
	UnlimitedQuota bool   `json:"unlimited_quota"`
	BudgetDuration string `json:"budget_duration"`
	TPMRPM         string `json:"tpm_rpm"`
	Tags           string `json:"tags"`
	Group          string `json:"group"`
	ModelLimits    string `json:"model_limits"`
	AllowIPs       string `json:"allow_ips"`
	ExpiredTime    int64  `json:"expired_time"`
}

// generateTokenKey 生成明文密钥："sk-" + 32 位随机 hex（128 bit 熵）。
func generateTokenKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// ListTokens 分页列表，可选 user_id 过滤（model.Token 的 key/key_hash 序列化豁免）。
func (h *Handler) ListTokens(c *gin.Context) {
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.Token{})
	if v := c.Query("user_id"); v != "" {
		q = q.Where("user_id = ?", v)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询令牌失败")
		return
	}
	var rows []model.Token
	if err := q.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询令牌失败")
		return
	}
	writeOK(c, gin.H{"items": rows, "total": total, "page": page, "page_size": pageSize})
}

// tokenMineView 是 GET /api/token/mine 的响应视图：面向登录用户的令牌自视
// 字段白名单——不含密钥材料（key/key_hash，model 层本已豁免，此处显式列举）
// 与管理配置字段（tpm_rpm/tags/allow_ips，由管理员经管理面维护）。
type tokenMineView struct {
	ID             int64  `json:"id"`
	UserID         int64  `json:"user_id"`
	Name           string `json:"name"`
	Status         int    `json:"status"`
	Quota          int64  `json:"quota"`
	RemainQuota    int64  `json:"remain_quota"`
	UnlimitedQuota bool   `json:"unlimited_quota"`
	BudgetDuration string `json:"budget_duration"`
	BudgetResetAt  int64  `json:"budget_reset_at"`
	Group          string `json:"group"`
	ModelLimits    string `json:"model_limits"`
	ExpiredTime    int64  `json:"expired_time"`
	CreatedTime    int64  `json:"created_time"`
	AccessedTime   int64  `json:"accessed_time"`
}

// ListMyTokens 当前登录用户名下令牌列表（登录态即可，docs/05 自服务组）：
// 所有权作用域强制取会话用户（user_id 查询参数被忽略，不可越权枚举他人令牌），
// 响应为 tokenMineView 白名单字段，分页形状与 /api/token 管理列表一致。
func (h *Handler) ListMyTokens(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.Token{}).Where("user_id = ?", u.ID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询令牌失败")
		return
	}
	var rows []model.Token
	if err := q.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询令牌失败")
		return
	}
	items := make([]tokenMineView, 0, len(rows))
	for _, t := range rows {
		items = append(items, tokenMineView{
			ID: t.ID, UserID: t.UserID, Name: t.Name, Status: t.Status,
			Quota: t.Quota, RemainQuota: t.RemainQuota, UnlimitedQuota: t.UnlimitedQuota,
			BudgetDuration: t.BudgetDuration, BudgetResetAt: t.BudgetResetAt,
			Group: t.Group, ModelLimits: t.ModelLimits, ExpiredTime: t.ExpiredTime,
			CreatedTime: t.CreatedTime, AccessedTime: t.AccessedTime,
		})
	}
	writeOK(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// CreateToken 创建令牌：校验归属用户存在，生成一次性明文密钥；
// 非 unlimited 且未显式传 remain 时缺省 remain=quota。
func (h *Handler) CreateToken(c *gin.Context) {
	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.UserID <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "user_id 必填")
		return
	}
	var u model.User
	if err := h.st.Read.First(&u, req.UserID).Error; err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", "归属用户不存在")
		return
	}
	plain, err := generateTokenKey()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "create_failed", "生成密钥失败")
		return
	}
	remain := req.RemainQuota
	if !req.UnlimitedQuota && remain == 0 && req.Quota > 0 {
		remain = req.Quota
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = u.Group // 用户分组作为令牌创建缺省组（M2-wave1 语义）
	}
	if group == "" {
		group = "default"
	}
	now := time.Now().Unix()
	tok := model.Token{
		UserID: req.UserID, Name: req.Name, Key: plain,
		KeyHash: gateway.HashKey(plain),
		Status:  req.Status, Quota: req.Quota, RemainQuota: remain,
		UnlimitedQuota: req.UnlimitedQuota, BudgetDuration: req.BudgetDuration,
		TPMRPM: req.TPMRPM, Tags: req.Tags, Group: group,
		ModelLimits: req.ModelLimits, AllowIPs: req.AllowIPs,
		ExpiredTime: req.ExpiredTime, CreatedTime: now,
	}
	if tok.Status == 0 {
		tok.Status = model.StatusEnabled
	}
	if tok.ExpiredTime == 0 {
		tok.ExpiredTime = model.EpochForever
	}
	if err := h.st.Write.Create(&tok).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "create_failed", "创建令牌失败")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "message": "",
		"data": gin.H{"token": tok, "key": plain}})
}

// UpdateToken 整对象幂等替换（身份与归属不可改）；写后失效鉴权缓存。
func (h *Handler) UpdateToken(c *gin.Context) {
	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var tok model.Token
	if err := h.st.Read.First(&tok, paramID(c)).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "令牌不存在")
		return
	}
	tok.Name = req.Name
	tok.Status = req.Status
	tok.Quota = req.Quota
	tok.RemainQuota = req.RemainQuota
	tok.UnlimitedQuota = req.UnlimitedQuota
	tok.BudgetDuration = req.BudgetDuration
	tok.TPMRPM = req.TPMRPM
	tok.Tags = req.Tags
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "default"
	}
	tok.Group = group
	tok.ModelLimits = req.ModelLimits
	tok.AllowIPs = req.AllowIPs
	tok.ExpiredTime = req.ExpiredTime
	if tok.Status == 0 {
		tok.Status = model.StatusEnabled
	}
	if tok.ExpiredTime == 0 {
		tok.ExpiredTime = model.EpochForever
	}
	if err := h.st.Write.Save(&tok).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "update_failed", "更新令牌失败")
		return
	}
	h.invalidateToken(tok.KeyHash)
	writeOK(c, tok)
}

// DeleteToken 删除令牌并失效鉴权缓存。
func (h *Handler) DeleteToken(c *gin.Context) {
	var tok model.Token
	if err := h.st.Read.First(&tok, paramID(c)).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "令牌不存在")
		return
	}
	if err := h.st.Write.Delete(&tok).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "删除令牌失败")
		return
	}
	h.invalidateToken(tok.KeyHash)
	writeOK(c, nil)
}

// invalidateToken 失效令牌鉴权缓存（gw 为 nil 时跳过，最小构造/测试）。
func (h *Handler) invalidateToken(keyHash string) {
	if h.gw != nil {
		h.gw.Auth().Invalidate(keyHash)
	}
}
