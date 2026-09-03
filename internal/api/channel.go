// channel.go 渠道管理端点（M2-wave1，docs/05）：root 权限 CRUD 与连通性测试。
//
// PUT 语义为整对象幂等写（吸取行业“裸对象丢 priority”教训）：请求体为完整对象，
// 业务字段全部显式覆盖（含零值），同一内容两次 PUT 结果一致；不做部分合并。
// 上游密钥为敏感列：JSON 序列化从不输出（json:"-"），响应以脱敏视图回显，
// 客户端持有的是脱敏值，故更新时 key 为空 = 保留旧值。
// 增删改后复位该渠道熔断计数，使新配置即时生效。
package api

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// channelTestTimeout 是渠道连通性测试的单次请求超时。
const channelTestTimeout = 10 * time.Second

// registerChannelRoutes 挂载 /api/channel 路由。
func (h *Handler) registerChannelRoutes(g *gin.RouterGroup) {
	g.GET("/channel", h.ListChannels)
	g.GET("/channel/:id", h.GetChannel)
	g.POST("/channel", h.CreateChannel)
	g.PUT("/channel/:id", h.UpdateChannel)
	g.DELETE("/channel/:id", h.DeleteChannel)
	g.POST("/channel/test/:id", h.TestChannel)
}

// channelRequest 是渠道创建/更新的完整请求对象（整对象写；不含 id 与时间戳）。
type channelRequest struct {
	Name          string `json:"name"`
	Type          int    `json:"type"`
	BaseURL       string `json:"base_url"`
	Key           string `json:"key"`
	Models        string `json:"models"`
	Priority      int64  `json:"priority"`
	Weight        int64  `json:"weight"`
	Status        int    `json:"status"`
	ParamOverride string `json:"param_override"`
}

// channelView 是渠道响应视图：密钥以脱敏形式回显。
type channelView struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Type          int    `json:"type"`
	BaseURL       string `json:"base_url"`
	KeyMasked     string `json:"key"`
	Models        string `json:"models"`
	Priority      int64  `json:"priority"`
	Weight        int64  `json:"weight"`
	Status        int    `json:"status"`
	ParamOverride string `json:"param_override"`
	CreatedTime   int64  `json:"created_time"`
	UpdatedTime   int64  `json:"updated_time"`
}

// maskKey 脱敏密钥：保留首 3 位与末 4 位，中间以 *** 代替；短值整体打码。
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "***"
	}
	return key[:3] + "***" + key[len(key)-4:]
}

// channelViewOf 构造脱敏视图。
func channelViewOf(ch model.Channel) channelView {
	return channelView{
		ID: ch.ID, Name: ch.Name, Type: ch.Type, BaseURL: ch.BaseURL,
		KeyMasked: maskKey(ch.Key), Models: ch.Models,
		Priority: ch.Priority, Weight: ch.Weight, Status: ch.Status,
		ParamOverride: ch.ParamOverride,
		CreatedTime:   ch.CreatedTime, UpdatedTime: ch.UpdatedTime,
	}
}

// ListChannels 分页列表（priority desc, id asc），可选 type/status 过滤。
func (h *Handler) ListChannels(c *gin.Context) {
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.Channel{})
	if v := c.Query("type"); v != "" {
		q = q.Where("type = ?", v)
	}
	if v := c.Query("status"); v != "" {
		q = q.Where("status = ?", v)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询渠道失败")
		return
	}
	var rows []model.Channel
	if err := q.Order("priority desc, id asc").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询渠道失败")
		return
	}
	items := make([]channelView, 0, len(rows))
	for _, ch := range rows {
		items = append(items, channelViewOf(ch))
	}
	writeOK(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// GetChannel 单个渠道（key 脱敏）。
func (h *Handler) GetChannel(c *gin.Context) {
	var ch model.Channel
	if err := h.st.Read.First(&ch, paramID(c)).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "渠道不存在")
		return
	}
	writeOK(c, channelViewOf(ch))
}

// CreateChannel 创建渠道。成功 201 返回脱敏视图。
func (h *Handler) CreateChannel(c *gin.Context) {
	var req channelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.BaseURL) == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "name 与 base_url 必填")
		return
	}
	if req.Type != model.ChannelTypeOpenAICompatible && req.Type != model.ChannelTypeAnthropic {
		writeErr(c, http.StatusBadRequest, "invalid_request", "type 仅支持 1(OpenAI 兼容) / 2(Anthropic)")
		return
	}
	now := time.Now().Unix()
	ch := model.Channel{
		Name: req.Name, Type: req.Type, BaseURL: req.BaseURL, Key: req.Key,
		Models: req.Models, Priority: req.Priority, Weight: req.Weight,
		Status: req.Status, ParamOverride: req.ParamOverride,
		CreatedTime: now, UpdatedTime: now,
	}
	if ch.Status == 0 {
		ch.Status = model.StatusEnabled
	}
	if err := h.st.Write.Create(&ch).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "create_failed", "创建渠道失败")
		return
	}
	h.resetChannel(ch.ID)
	c.JSON(http.StatusCreated, gin.H{"success": true, "message": "", "data": channelViewOf(ch)})
}

// UpdateChannel 整对象幂等替换：业务字段全量覆盖（含零值），key 为空保留旧值。
func (h *Handler) UpdateChannel(c *gin.Context) {
	id := paramID(c)
	var req channelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var ch model.Channel
	if err := h.st.Read.First(&ch, id).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "渠道不存在")
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.BaseURL) == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "name 与 base_url 必填")
		return
	}
	if req.Type != model.ChannelTypeOpenAICompatible && req.Type != model.ChannelTypeAnthropic {
		writeErr(c, http.StatusBadRequest, "invalid_request", "type 仅支持 1(OpenAI 兼容) / 2(Anthropic)")
		return
	}
	ch.Name = req.Name
	ch.Type = req.Type
	ch.BaseURL = req.BaseURL
	if req.Key != "" { // 客户端持有脱敏回显，空值 = 保留旧密钥
		ch.Key = req.Key
	}
	ch.Models = req.Models
	ch.Priority = req.Priority
	ch.Weight = req.Weight
	ch.Status = req.Status
	ch.ParamOverride = req.ParamOverride
	ch.UpdatedTime = time.Now().Unix()
	if err := h.st.Write.Save(&ch).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "update_failed", "更新渠道失败")
		return
	}
	h.resetChannel(ch.ID)
	writeOK(c, channelViewOf(ch))
}

// DeleteChannel 删除渠道并复位其熔断状态。
func (h *Handler) DeleteChannel(c *gin.Context) {
	id := paramID(c)
	var ch model.Channel
	if err := h.st.Read.First(&ch, id).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "渠道不存在")
		return
	}
	if err := h.st.Write.Delete(&ch).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "删除渠道失败")
		return
	}
	h.resetChannel(id)
	writeOK(c, nil)
}

// TestChannel 连通性测试：向上游发一条最小 GET /v1/models 请求（不消耗 tokens），
// 返回 HTTP 状态与耗时。测试结果不影响熔断状态，也不落库。
func (h *Handler) TestChannel(c *gin.Context) {
	var ch model.Channel
	if err := h.st.Read.First(&ch, paramID(c)).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "渠道不存在")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	req, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		writeOK(c, gin.H{"success": false, "status_code": 0, "time_ms": int64(0),
			"message": "构造测试请求失败: " + err.Error()})
		return
	}
	if ch.Type == model.ChannelTypeAnthropic {
		req.Header.Set("x-api-key", ch.Key)
		req.Header.Set("anthropic-version", "2023-01-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+ch.Key)
	}
	client := &http.Client{Timeout: channelTestTimeout}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		writeOK(c, gin.H{"success": false, "status_code": 0,
			"time_ms": elapsed.Milliseconds(), "message": "上游不可达: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	msg := ""
	if !ok {
		msg = "上游返回非 2xx 状态"
	}
	writeOK(c, gin.H{"success": ok, "status_code": resp.StatusCode,
		"time_ms": elapsed.Milliseconds(), "message": msg})
}
