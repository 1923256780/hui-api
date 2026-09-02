package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/override"
	"github.com/1923256780/hui-api/internal/relay"
	"github.com/1923256780/hui-api/internal/store"
)

// Gateway 运行参数默认值（可经 options 运行轨覆盖）。
const (
	// DefaultMaxBodyBytes 入口请求体上限（pre-call 检查，超出 413）。
	DefaultMaxBodyBytes = 32 << 20
	// upstreamTimeout 每跳上游请求超时（流式连接也受其约束，由 client Timeout 生效）。
	upstreamTimeout = 120 * time.Second
)

// OptionKeyMaxBodyBytes 是 options 运行轨的请求体上限键（热更）。
const OptionKeyMaxBodyBytes = "relay.max_body_bytes"

// Gateway 是转发链路编排器：鉴权 → pre-call → 选择 → override → 转发 → 重试。
type Gateway struct {
	st      *store.Store
	rt      *config.Runtime
	auth    *TokenAuth
	sel     *Selector
	breaker *BreakerRegistry
	client  *http.Client
}

// New 构造网关。
func New(st *store.Store, rt *config.Runtime) *Gateway {
	return &Gateway{
		st:      st,
		rt:      rt,
		auth:    NewTokenAuth(st),
		sel:     NewSelector(st),
		breaker: NewBreakerRegistry(DefaultBreakerConfig(), nil),
		client: &http.Client{
			Timeout: upstreamTimeout,
			// 不跟随重定向：转发语义下 3xx 视为上游异常行为。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Auth 暴露鉴权器（M3 管理面写时失效挂接点）。
func (g *Gateway) Auth() *TokenAuth { return g.auth }

// Breaker 暴露熔断注册表（M3 管理面复位挂接点）。
func (g *Gateway) Breaker() *BreakerRegistry { return g.breaker }

// maxBodyBytes 读取入口请求体上限（运行轨热更，缺省 32MB）。
func (g *Gateway) maxBodyBytes() int64 {
	if g.rt == nil {
		return DefaultMaxBodyBytes
	}
	v := g.rt.GetInt64(OptionKeyMaxBodyBytes, DefaultMaxBodyBytes)
	if v <= 0 {
		return DefaultMaxBodyBytes
	}
	return v
}

// Serve 处理一次转发请求：完整执行鉴权、pre-call、选择、改写、转发与重试。
// 这是所有转发面端点的唯一编排入口，协议差异全部收敛在 proto 实现里。
func (g *Gateway) Serve(c *gin.Context, proto relay.Protocol) {
	// ---- 1. 鉴权：提取客户端密钥 → key_hash 校验（缓存/库）。
	key, ok := proto.ExtractKey(c)
	if !ok || strings.TrimSpace(key) == "" {
		proto.WriteError(c, http.StatusUnauthorized, "missing_api_key", "缺少 API 密钥")
		return
	}
	tok, err := g.auth.Authenticate(key)
	if err != nil {
		switch {
		case errors.Is(err, ErrTokenDisabled):
			proto.WriteError(c, http.StatusForbidden, "token_disabled", "令牌已禁用")
		case errors.Is(err, ErrTokenExpired):
			proto.WriteError(c, http.StatusForbidden, "token_expired", "令牌已过期")
		default:
			proto.WriteError(c, http.StatusUnauthorized, "invalid_api_key", "API 密钥无效")
		}
		return
	}

	// ---- 2. pre-call：请求体大小限制（本地快速失败，不触上游）。
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, g.maxBodyBytes()+1))
	if err != nil {
		proto.WriteError(c, http.StatusBadRequest, "read_body_failed", "读取请求体失败")
		return
	}
	if int64(len(raw)) > g.maxBodyBytes() {
		proto.WriteError(c, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过大小限制")
		return
	}

	// ---- 3. 请求解析（协议归一）。
	pr, err := proto.ParseBody(raw)
	if err != nil {
		proto.WriteError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(pr.Model) == "" {
		proto.WriteError(c, http.StatusBadRequest, "invalid_request", "缺少 model 字段")
		return
	}

	// ---- 4. 重试循环：typed retry，重试仅限首字节前（2xx 交给 Respond 后不再重试）。
	excluded := make(map[int64]bool, MaxExcluded) // 排除集：跨跳累积、按渠道 ID 去重
	attempt := 0
	for {
		ch, err := g.sel.Pick(proto.ChannelType(), pr.Model, excluded)
		if err != nil {
			log.Printf("[relay] %s 查询候选渠道失败: %v", proto.Name(), err)
			proto.WriteError(c, http.StatusInternalServerError, "gateway_error", "渠道查询失败")
			return
		}
		if ch == nil {
			// 无可用渠道（含全部熔断中 / 协议与渠道组不匹配）：503 语义错误。
			proto.WriteError(c, http.StatusServiceUnavailable, "no_available_channel",
				"模型 "+pr.Model+" 暂无可用渠道")
			return
		}

		// 渠道级参数改写（channels.param_override）。
		payload, err := override.Apply(raw, ch.ParamOverride)
		if err != nil {
			// 渠道配置错误：隔离该渠道换点重试（不计入上游错误分类）。
			log.Printf("[relay] 渠道 %d param_override 配置错误: %v", ch.ID, err)
			g.breaker.OnFailure(ch.ID, ClassUnknown)
			if stop := g.markExcludedAndDecide(excluded, ch.ID); !stop {
				continue
			}
			proto.WriteError(c, http.StatusBadGateway, "upstream_error", "渠道参数配置错误")
			return
		}

		upReq, err := proto.PrepareUpstream(ch, proto.UpstreamPath(c), payload, c.Request.Header)
		if err != nil {
			log.Printf("[relay] 渠道 %d 构造上游请求失败: %v", ch.ID, err)
			proto.WriteError(c, http.StatusBadGateway, "upstream_error", "构造上游请求失败")
			return
		}

		resp, err := g.client.Do(upReq)
		if err != nil {
			class := ClassifyNetworkErr(err)
			log.Printf("[relay] 渠道 %d 网络错误 class=%s: %v", ch.ID, class, err)
			g.breaker.OnFailure(ch.ID, class)
			policy := PolicyFor(class)
			if policy.Retryable && len(excluded) < MaxExcluded {
				WaitBackoff(policy, attempt)
				attempt++
				excluded[ch.ID] = true
				continue
			}
			proto.WriteError(c, http.StatusBadGateway, "upstream_unreachable", "上游不可达")
			return
		}

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, ErrBodyLimit))
			_ = resp.Body.Close()
			class := ClassifyStatus(resp.StatusCode, body)
			log.Printf("[relay] 渠道 %d 上游错误 class=%s status=%d model=%s token=%d",
				ch.ID, class, resp.StatusCode, pr.Model, tok.ID)
			g.breaker.OnFailure(ch.ID, class)
			policy := PolicyFor(class)
			if policy.Retryable && len(excluded) < MaxExcluded {
				WaitBackoff(policy, attempt)
				attempt++
				excluded[ch.ID] = true
				continue
			}
			// 重试穷尽或不可重试：透传上游错误（保持客户端 SDK 无感）；
			// Auth 类除外——渠道密钥错误不应让客户端误判为自己的密钥问题。
			if class == ClassAuth {
				proto.WriteError(c, http.StatusBadGateway, "upstream_auth_failed", "上游渠道鉴权失败")
				return
			}
			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			c.Data(resp.StatusCode, contentType, body)
			return
		}

		// ---- 5. 2xx：交给协议适配层转发（流式逐事件 flush / 非流式透传）。
		// 从此处起首字节即将写出，任何失败都不再重试。
		g.breaker.OnSuccess(ch.ID)
		usage, err := proto.Respond(c, resp, pr)
		if err != nil {
			log.Printf("[relay] 渠道 %d 响应转发失败 model=%s: %v", ch.ID, pr.Model, err)
		}
		log.Printf("[relay] %s model=%s channel=%d token=%d stream=%v prompt=%d completion=%d",
			proto.Name(), pr.Model, ch.ID, tok.ID, pr.Stream, usage.PromptTokens, usage.CompletionTokens)
		return
	}
}

// markExcludedAndDecide 把渠道加入排除集；返回 false 表示仍可继续重试。
func (g *Gateway) markExcludedAndDecide(excluded map[int64]bool, id int64) bool {
	excluded[id] = true
	return len(excluded) >= MaxExcluded
}

// ---- /v1/models ----

// OptionKeyVirtualGroups 是虚拟模型组配置键：JSON 对象 {"组名": ["成员",...]}。
// /v1/models 只返回组名（组内成员不展开，见任务范围）。
const OptionKeyVirtualGroups = "relay.virtual_model_groups"

// ModelInfo 是 /v1/models 的单个条目。
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// ListModels 返回全部启用渠道的模型并集 + 虚拟模型组名（组内不展开）。
// 供 GET /v1/models 使用；结果按名称排序去重。
func (g *Gateway) ListModels() ([]ModelInfo, error) {
	channels, err := g.st.GetEnabledChannels()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var names []string
	for _, ch := range channels {
		for _, m := range store.ChannelModelList(ch) {
			if m == "*" || seen[m] {
				continue // 通配渠道不展开为具体模型名
			}
			seen[m] = true
			names = append(names, m)
		}
	}
	for _, group := range g.virtualGroupNames() {
		if !seen[group] {
			seen[group] = true
			names = append(names, group)
		}
	}
	sort.Strings(names)
	out := make([]ModelInfo, 0, len(names))
	for _, n := range names {
		out = append(out, ModelInfo{ID: n, Object: "model", OwnedBy: "hui-api"})
	}
	return out, nil
}

// HandleModels 是 GET /v1/models 的 gin handler（转发面，令牌鉴权）。
func (g *Gateway) HandleModels(c *gin.Context) {
	key := c.Request.Header.Get("Authorization")
	key = strings.TrimPrefix(key, "Bearer ")
	if strings.TrimSpace(key) == "" {
		key = c.Request.Header.Get("x-api-key")
	}
	if strings.TrimSpace(key) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"message": "缺少 API 密钥", "type": "invalid_request_error", "code": "missing_api_key"},
		})
		return
	}
	if _, err := g.auth.Authenticate(key); err != nil {
		switch {
		case errors.Is(err, ErrTokenDisabled), errors.Is(err, ErrTokenExpired):
			c.JSON(http.StatusForbidden, gin.H{
				"error": gin.H{"message": err.Error(), "type": "invalid_request_error", "code": "token_unavailable"},
			})
		default:
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"message": "API 密钥无效", "type": "invalid_request_error", "code": "invalid_api_key"},
			})
		}
		return
	}
	models, err := g.ListModels()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": "查询模型失败", "type": "gateway_error", "code": "gateway_error"},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
}

// virtualGroupNames 从运行轨读取虚拟模型组配置并返回组名列表。
func (g *Gateway) virtualGroupNames() []string {
	if g.rt == nil {
		return nil
	}
	raw, ok := g.rt.Get(OptionKeyVirtualGroups)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var groups map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		log.Printf("[relay] 虚拟模型组配置非法（%s）: %v", OptionKeyVirtualGroups, err)
		return nil
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	return names
}
