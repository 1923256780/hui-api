// log.go 日志查询端点（M2-wave1，docs/05）：root 权限分页查询 /api/log，
// 支持按用户/令牌/渠道/模型/时间区间过滤；M3-wave4 新增登录态个人视角
// GET /api/log/mine（会话作用域 + logMineView 白名单字段）。写入口在转发面
// hook（异步旁路），两处均只读。
package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// registerLogRoutes 挂载 /api/log 路由。
func (h *Handler) registerLogRoutes(g *gin.RouterGroup) {
	g.GET("/log", h.ListLogs)
}

// logQuery 可选过滤参数；时间戳为 Unix 秒（start/end 闭区间）。
type logQuery struct {
	UserID         int64
	TokenID        int64
	ChannelID      int64
	ModelName      string
	StartTimestamp int64
	EndTimestamp   int64
}

// parseLogQuery 解析查询串过滤参数（非法值忽略该条件，宽松语义）。
func parseLogQuery(c *gin.Context) logQuery {
	var q logQuery
	q.UserID, _ = strconv.ParseInt(c.Query("user_id"), 10, 64)
	q.TokenID, _ = strconv.ParseInt(c.Query("token_id"), 10, 64)
	q.ChannelID, _ = strconv.ParseInt(c.Query("channel_id"), 10, 64)
	q.ModelName = c.Query("model_name")
	q.StartTimestamp, _ = strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	q.EndTimestamp, _ = strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	return q
}

// ListLogs 分页查询计费日志，id desc（最新在前）。
// channel_id 条件在 hook 未回填 channel_id 前恒不命中（wave1 仅链路就绪）。
func (h *Handler) ListLogs(c *gin.Context) {
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.Log{})
	f := parseLogQuery(c)
	if f.UserID > 0 {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.TokenID > 0 {
		q = q.Where("token_id = ?", f.TokenID)
	}
	if f.ChannelID > 0 {
		q = q.Where("channel_id = ?", f.ChannelID)
	}
	if f.ModelName != "" {
		q = q.Where("model_name = ?", f.ModelName)
	}
	if f.StartTimestamp > 0 {
		q = q.Where("created_time >= ?", f.StartTimestamp)
	}
	if f.EndTimestamp > 0 {
		q = q.Where("created_time <= ?", f.EndTimestamp)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询日志失败")
		return
	}
	var rows []model.Log
	if err := q.Order("id desc").Offset((page - 1) * pageSize).
		Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询日志失败")
		return
	}
	writeOK(c, gin.H{"items": rows, "total": total, "page": page, "page_size": pageSize})
}

// logMineView 是 GET /api/log/mine 的响应视图（M3-wave4）：登录用户计费日志
// 自视字段白名单，照 tokenMineView 模式显式列举——不含渠道归属（channel_id，
// 内部路由语义属管理面）与 user_id（恒为会话用户，冗余）；token_id 为数字
// ID，logs 表本不含令牌名/密钥材料（tokens.key/key_hash 在令牌域隔离）。
type logMineView struct {
	ID               int64  `json:"id"`
	TokenID          int64  `json:"token_id"`
	Protocol         string `json:"protocol"`
	ModelName        string `json:"model_name"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Quota            int64  `json:"quota"`
	UseTime          int64  `json:"use_time"`
	IsStream         bool   `json:"is_stream"`
	Detail           string `json:"detail"`
	CreatedTime      int64  `json:"created_time"`
}

// ListMyLogs 当前登录用户本人的计费日志分页（登录态，id 降序）：会话作用域
// 强制取会话用户（user_id 查询参数被忽略，不可越权枚举他人日志），响应为
// logMineView 白名单字段，分页形状与 /api/log 管理列表一致；过滤参数为
// model_name 与 start/end_timestamp（Unix 秒闭区间），照 /api/log 惯例宽松
// 解析（非法值忽略该条件）。本仓库 logs 表无类型列，全部记录均为计费/入账
// 事实日志（consume 语义），故不做协议过滤。
func (h *Handler) ListMyLogs(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.Log{}).Where("user_id = ?", u.ID)
	if v := c.Query("model_name"); v != "" {
		q = q.Where("model_name = ?", v)
	}
	if v, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64); v > 0 {
		q = q.Where("created_time >= ?", v)
	}
	if v, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64); v > 0 {
		q = q.Where("created_time <= ?", v)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询日志失败")
		return
	}
	var rows []model.Log
	if err := q.Order("id desc").Offset((page - 1) * pageSize).
		Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询日志失败")
		return
	}
	items := make([]logMineView, 0, len(rows))
	for _, l := range rows {
		items = append(items, logMineView{
			ID: l.ID, TokenID: l.TokenID, Protocol: l.Protocol,
			ModelName: l.ModelName, PromptTokens: l.PromptTokens,
			CompletionTokens: l.CompletionTokens, Quota: l.Quota,
			UseTime: l.UseTime, IsStream: l.IsStream, Detail: l.Detail,
			CreatedTime: l.CreatedTime,
		})
	}
	writeOK(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}
