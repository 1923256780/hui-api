// ratelimit.go 是转发链路的限流挂接（M2-wave1，docs/05 管理面契约）：
//
//   - 全局请求限流：ModelRequestRateLimit* 语义（Enabled/DurationMinutes/Count/SuccessCount，
//     行业通用命名），按客户端 IP 分桶；
//   - 分组请求限流：ModelRequestRateLimitGroup JSON {"组名": [最大请求数, 最大成功数]}，
//     分组配置覆盖全局 Count/SuccessCount、共用 DurationMinutes 周期；
//   - 令牌级 TPM/RPM：tokens.tpm_rpm JSON {"tpm":n,"rpm":n}，滑动窗口（internal/ratelimit）；
//   - 超限统一 429 + Retry-After；本机限流发生在渠道选择之前，不计入上游熔断。
package gateway

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/relay"
)

// options 运行轨的请求限流键（热更；命名沿用行业通用语义）。
const (
	// OptionKeyRateLimitEnabled 全局限流开关（bool）。
	OptionKeyRateLimitEnabled = "ModelRequestRateLimitEnabled"
	// OptionKeyRateLimitDurationMinutes 限流窗口时长（分钟，整数）。
	OptionKeyRateLimitDurationMinutes = "ModelRequestRateLimitDurationMinutes"
	// OptionKeyRateLimitCount 窗口内最大请求数（0=不限）。
	OptionKeyRateLimitCount = "ModelRequestRateLimitCount"
	// OptionKeyRateLimitSuccessCount 窗口内最大成功数（0=不限）。
	OptionKeyRateLimitSuccessCount = "ModelRequestRateLimitSuccessCount"
	// OptionKeyRateLimitGroup 分组覆盖 JSON：{"组名": [最大请求数, 最大成功数]}。
	OptionKeyRateLimitGroup = "ModelRequestRateLimitGroup"
)

// requestLimitConfig 是一次请求解析出的全局/分组请求限流配置。
type requestLimitConfig struct {
	Enabled     bool
	Window      time.Duration
	MaxRequests int
	MaxSuccess  int
	Scope       string // 限流身份命名空间："g"（全局）/"grp:<组名>"（分组覆盖生效时）
}

// requestLimitConfig 解析请求限流配置：Enabled 关闭时返回零值；分组 JSON 命中
// 当前令牌组时覆盖全局 Count/SuccessCount（0 表示该维度不限），共用全局周期。
func (g *Gateway) requestLimitConfig(group string) requestLimitConfig {
	if g.rt == nil {
		return requestLimitConfig{}
	}
	if !g.rt.GetBool(OptionKeyRateLimitEnabled, false) {
		return requestLimitConfig{}
	}
	minutes := g.rt.GetInt64(OptionKeyRateLimitDurationMinutes, 1)
	if minutes < 1 {
		minutes = 1
	}
	cfg := requestLimitConfig{
		Enabled:     true,
		Window:      time.Duration(minutes) * time.Minute,
		MaxRequests: int(g.rt.GetInt64(OptionKeyRateLimitCount, 0)),
		MaxSuccess:  int(g.rt.GetInt64(OptionKeyRateLimitSuccessCount, 0)),
		Scope:       "g",
	}
	if entry, ok := groupRateLimit(g.rt, group); ok {
		cfg.MaxRequests = entry.maxRequests
		cfg.MaxSuccess = entry.maxSuccess
		cfg.Scope = "grp:" + group
	}
	return cfg
}

// groupLimitEntry 是分组限流单条目（数组两元素）。
type groupLimitEntry struct {
	maxRequests int
	maxSuccess  int
}

// groupRateLimit 解析分组限流 JSON 并返回当前组条目。配置非法或组未配置时
// 返回 false（回落全局配置）；条目长度必须为 2 且数值非负，否则视为非法跳过。
func groupRateLimit(rt *config.Runtime, group string) (groupLimitEntry, bool) {
	raw, ok := rt.Get(OptionKeyRateLimitGroup)
	if !ok || strings.TrimSpace(raw) == "" {
		return groupLimitEntry{}, false
	}
	var m map[string][]int64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		log.Printf("[ratelimit] 分组限流配置非法（%s）: %v", OptionKeyRateLimitGroup, err)
		return groupLimitEntry{}, false
	}
	arr, ok := m[group]
	if !ok || len(arr) != 2 || arr[0] < 0 || arr[1] < 0 {
		return groupLimitEntry{}, false
	}
	return groupLimitEntry{maxRequests: int(arr[0]), maxSuccess: int(arr[1])}, true
}

// tokenGroup 返回令牌运行时归属组（tokens.group；缺省 default，与计费组倍率同源）。
func tokenGroup(tok *model.Token) string {
	if g := strings.TrimSpace(tok.Group); g != "" {
		return g
	}
	return billing.DefaultGroup
}

// tokenAllowsModel 判断令牌模型白名单：ModelLimits 空 = 不限；逗号分隔精确匹配。
func tokenAllowsModel(tok *model.Token, modelName string) bool {
	raw := strings.TrimSpace(tok.ModelLimits)
	if raw == "" {
		return true
	}
	for _, m := range strings.Split(raw, ",") {
		if strings.TrimSpace(m) == modelName {
			return true
		}
	}
	return false
}

// tokenAllowsIP 判断令牌 IP 白名单：AllowIPs 空 = 不限；条目支持单 IP 与 CIDR
// （10.0.0.1 或 10.0.0.0/8）。客户端 IP 无法解析时从严拒绝；非法条目记日志跳过。
func tokenAllowsIP(tok *model.Token, clientIP string) bool {
	raw := strings.TrimSpace(tok.AllowIPs)
	if raw == "" {
		return true
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		log.Printf("[ratelimit] 客户端 IP 无法解析，按白名单拒绝: %q", clientIP)
		return false
	}
	addr = addr.Unmap()
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			if p.Contains(addr) {
				return true
			}
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			if a.Unmap() == addr {
				return true
			}
			continue
		}
		log.Printf("[ratelimit] 令牌 %d IP 白名单条目非法，跳过: %q", tok.ID, e)
	}
	return false
}

// tokenTPMRPM 是令牌级限流上限（tokens.tpm_rpm JSON）。
type tokenTPMRPM struct {
	TPM int // 每分钟 tokens 用量上限（0=不限）
	RPM int // 每分钟请求数上限（0=不限）
}

// parseTPMRPM 解析令牌 tpm_rpm JSON；空值/非法/负值均视为未配置（不限）。
func parseTPMRPM(raw string) (tokenTPMRPM, bool) {
	if strings.TrimSpace(raw) == "" {
		return tokenTPMRPM{}, false
	}
	var v struct {
		TPM int `json:"tpm"`
		RPM int `json:"rpm"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v.TPM < 0 || v.RPM < 0 {
		return tokenTPMRPM{}, false
	}
	return tokenTPMRPM{TPM: v.TPM, RPM: v.RPM}, true
}

// tokenLimitKey 生成令牌级限流的窗口 key（与全局/分组 IP 命名空间隔离）。
func tokenLimitKey(tokenID int64) string {
	return "tok:" + strconv.FormatInt(tokenID, 10)
}

// writeRateLimited 统一写出 429 语义错误与 Retry-After 头（秒，向上取整，最小 1）。
func writeRateLimited(c *gin.Context, proto relay.Protocol, retry time.Duration) {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	c.Header("Retry-After", strconv.Itoa(secs))
	proto.WriteError(c, http.StatusTooManyRequests, "rate_limit_exceeded", "请求触发限流，请稍后重试")
}
