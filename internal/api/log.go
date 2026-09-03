// log.go 日志查询端点（M2-wave1，docs/05）：root 权限分页查询，
// 支持按用户/令牌/渠道/模型/时间区间过滤。写入口在转发面 hook（异步旁路），
// 此处只读。
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
