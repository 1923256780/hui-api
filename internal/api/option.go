// option.go 运行轨配置端点（M2-wave1，docs/05）：GET 全量 / PUT 批量写。
// 写入走键白名单（防止误改 schema_version 等内部键），值长度受限；
// 成功后触发 config.Runtime.Reload() 热生效——计费、限流与网关参数免重启，
// 在途请求仍使用各自的价格快照口径（docs/04）。
//
// M3-wave1 扩展：白名单新增商业化前缀（注册/SMTP/Turnstile/OAuth/支付/邀请/充值）；
// 敏感键读取脱敏（password/secret 子串 → "******"），PUT 收到哨兵值
// "******" 视为「保持旧值」跳过不覆盖（前端脱敏回显原样提交的幂等语义）。
package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
)

// optionValueMaxLen 是单个 option 值的长度上限（价格/限流 JSON 足够，防滥用）。
const optionValueMaxLen = 2048

// optionMaskedSentinel 是敏感键的脱敏回显哨兵：PUT 收到该值表示保持旧值。
const optionMaskedSentinel = "******"

// registerOptionRoutes 挂载 /api/option 路由。
func (h *Handler) registerOptionRoutes(g *gin.RouterGroup) {
	g.GET("/option", h.ListOptions)
	g.PUT("/option", h.UpdateOptions)
}

// allowedOptionKey 管理面可写键白名单：
//   - 模块命名空间前缀：relay.*（网关参数）、billing_setting.*（计费显式配置）、
//     hooks.*（观测旁路，M2-wave3）；M3-wave1 商业化前缀：smtp.*（邮件）、
//     register.*（注册）、oauth.*（第三方登录配置）、turnstile.*（人机校验）、
//     epay.* 与 stripe.*（支付网关，wave2 对接）、aff.*（邀请返利）、topup.*（充值）；
//   - 行业通用计费/限流精确键（与 gateway/billing 包常量同源）。
func allowedOptionKey(key string) bool {
	for _, p := range []string{
		"relay.", "billing_setting.", "hooks.",
		"smtp.", "register.", "oauth.", "turnstile.", "epay.", "stripe.", "aff.", "topup.",
	} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	switch key {
	case billing.OptionKeyModelRatio,
		billing.OptionKeyCompletionRatio,
		billing.OptionKeyGroupRatio,
		gateway.OptionKeyRateLimitEnabled,
		gateway.OptionKeyRateLimitDurationMinutes,
		gateway.OptionKeyRateLimitCount,
		gateway.OptionKeyRateLimitSuccessCount,
		gateway.OptionKeyRateLimitGroup:
		return true
	}
	return false
}

// isSensitiveOptionKey 判断键是否承载敏感值：键名含 password 或 secret 子串
// （不区分大小写，secret 覆盖 secret_key）→ 读取脱敏为哨兵值。
func isSensitiveOptionKey(key string) bool {
	lk := strings.ToLower(key)
	return strings.Contains(lk, "password") || strings.Contains(lk, "secret")
}

// ListOptions 返回全部 options（读取不做白名单过滤，便于诊断）。
// 敏感键的值脱敏为 "******"（库内明文不变，仅出口脱敏）。
func (h *Handler) ListOptions(c *gin.Context) {
	var rows []model.Option
	if err := h.st.Read.Order("key").Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询配置失败")
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		v := r.Value
		if isSensitiveOptionKey(r.Key) {
			v = optionMaskedSentinel
		}
		items = append(items, gin.H{"key": r.Key, "value": v})
	}
	writeOK(c, gin.H{"items": items, "version": h.rt.Version()})
}

// UpdateOptions 批量写 options（{"options": {key: value}}），全部校验通过后
// 单事务落库并触发热更；任一键非法则整体拒绝（不做部分写入）。
// 值为哨兵 "******" 的键视为「保持旧值」跳过不覆盖（敏感键脱敏回显幂等）。
func (h *Handler) UpdateOptions(c *gin.Context) {
	var req struct {
		Options map[string]string `json:"options"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Options) == 0 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "缺少 options 对象")
		return
	}
	for k, v := range req.Options {
		if !allowedOptionKey(k) {
			writeErr(c, http.StatusBadRequest, "option_forbidden", "键不在可写白名单: "+k)
			return
		}
		if len(v) > optionValueMaxLen {
			writeErr(c, http.StatusBadRequest, "option_too_long", "值超过长度上限: "+k)
			return
		}
	}
	// 哨兵过滤：校验先行（哨兵键也要过白名单），随后整体剔除不落库。
	updates := make(map[string]string, len(req.Options))
	for k, v := range req.Options {
		if v == optionMaskedSentinel {
			continue
		}
		updates[k] = v
	}
	if len(updates) > 0 {
		if err := h.st.SetOptions(updates); err != nil {
			writeErr(c, http.StatusInternalServerError, "option_write_failed", "写入配置失败")
			return
		}
	}
	if err := h.rt.Reload(); err != nil {
		// 落库成功但热更失败：下次重启仍会加载新值；显式报错让管理端感知。
		writeErr(c, http.StatusInternalServerError, "reload_failed", "写入成功但热加载失败: "+err.Error())
		return
	}
	writeOK(c, gin.H{"version": h.rt.Version(), "updated": len(updates)})
}
