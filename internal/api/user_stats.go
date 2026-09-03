// user_stats.go 自服务统计端点 GET /api/user/stats（M2 收官 Task #19，docs/05）：
// 普通用户数据看板的数据源。服务端 SQL 聚合 logs 表「今日」区间
// [本地 0 点, 当前] 的请求/消耗/tokens 汇总与模型分布，替代普通用户
// 直调管理面 GET /api/log（root 专属，403）的旧路径。
// 作用域恒为会话用户（user_id 查询参数被忽略），响应不含任何管理维度
// （channel/token/用户标识），不泄露他人数据。
package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// userStatsAggRow 是 logs 表聚合行的扫描载体（列别名对应 SELECT 语句）。
type userStatsAggRow struct {
	ModelName        string `gorm:"column:model_name" json:"model_name"`
	Requests         int64  `gorm:"column:requests" json:"requests"`
	PromptTokens     int64  `gorm:"column:prompt_tokens" json:"prompt_tokens"`
	CompletionTokens int64  `gorm:"column:completion_tokens" json:"completion_tokens"`
	Quota            int64  `gorm:"column:quota" json:"quota"`
}

// GetUserStats 当前登录用户今日统计（登录态自服务）：
// 汇总一次 + 模型分布一次（按 quota 降序，上限 100 个模型封顶响应体）。
// SUM 对空集返回 NULL，COALESCE 归零保证「无日志 = 全零空态」而非错误。
func (h *Handler) GetUserStats(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	end := now.Unix()
	scope := "user_id = ? AND created_time >= ? AND created_time <= ?"
	args := []any{u.ID, start, end}

	var total userStatsAggRow
	if err := h.st.Read.Model(&model.Log{}).
		Select("'' AS model_name, COUNT(*) AS requests, COALESCE(SUM(prompt_tokens),0) AS prompt_tokens, "+
			"COALESCE(SUM(completion_tokens),0) AS completion_tokens, COALESCE(SUM(quota),0) AS quota").
		Where(scope, args...).Scan(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "统计查询失败")
		return
	}
	var rows []userStatsAggRow
	if err := h.st.Read.Model(&model.Log{}).
		Select("model_name, COUNT(*) AS requests, COALESCE(SUM(prompt_tokens),0) AS prompt_tokens, "+
			"COALESCE(SUM(completion_tokens),0) AS completion_tokens, COALESCE(SUM(quota),0) AS quota").
		Where(scope, args...).
		Group("model_name").Order("quota desc").Limit(100).
		Scan(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "统计查询失败")
		return
	}
	writeOK(c, gin.H{
		"start_timestamp":   start,
		"end_timestamp":     end,
		"requests":          total.Requests,
		"prompt_tokens":     total.PromptTokens,
		"completion_tokens": total.CompletionTokens,
		"tokens":            total.PromptTokens + total.CompletionTokens,
		"quota":             total.Quota,
		"models":            rows,
	})
}
