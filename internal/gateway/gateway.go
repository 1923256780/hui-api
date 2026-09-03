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

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/override"
	"github.com/1923256780/hui-api/internal/ratelimit"
	"github.com/1923256780/hui-api/internal/relay"
	"github.com/1923256780/hui-api/internal/store"
)

// Gateway 运行参数默认值（可经 options 运行轨覆盖）。
const (
	// DefaultMaxBodyBytes 入口请求体上限（pre-call 检查，超出 413）。
	DefaultMaxBodyBytes = 32 << 20
	// upstreamTimeout 每跳上游请求超时（流式连接也受其约束，由 client Timeout 生效）。
	upstreamTimeout = 120 * time.Second
	// logCloseTimeout 优雅停机时异步日志排空等待窗口。
	logCloseTimeout = 2 * time.Second
)

// OptionKeyMaxBodyBytes 是 options 运行轨的请求体上限键（热更）。
const OptionKeyMaxBodyBytes = "relay.max_body_bytes"

// Gateway 是转发链路编排器：鉴权 → 计费预扣 → pre-call → 选择 → override →
// 转发 → 结算 → 日志。计费内核在 internal/billing，本层只编排不实现计费公式。
type Gateway struct {
	st      *store.Store
	rt      *config.Runtime
	auth    *TokenAuth
	sel     *Selector
	breaker *BreakerRegistry
	client  *http.Client
	price   *billing.Engine         // 计费引擎（价格解析与三模式求值）
	ledger  *billing.Ledger         // 预扣费账本（冻结/多退少补/退款）
	logs    *billing.AsyncLogWriter // 请求日志异步批量落库
	rl      *ratelimit.Limiter      // 请求/令牌限流（滑动窗口，M2-wave1）
}

// New 构造网关。pricer 为已通过内置价单校验的计费引擎（main 启动时 fail-fast 构造）。
func New(st *store.Store, rt *config.Runtime, pricer *billing.Engine) *Gateway {
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
		price:  pricer,
		ledger: billing.NewLedger(st),
		logs:   billing.NewAsyncLogWriter(st),
		rl:     ratelimit.New(nil),
	}
}

// Auth 暴露鉴权器（M3 管理面写时失效挂接点）。
func (g *Gateway) Auth() *TokenAuth { return g.auth }

// Breaker 暴露熔断注册表（M3 管理面复位挂接点）。
func (g *Gateway) Breaker() *BreakerRegistry { return g.breaker }

// Close 优雅停机排空异步日志（main 收尾调用）。
func (g *Gateway) Close() { g.logs.Close(logCloseTimeout) }

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

// Serve 处理一次转发请求：完整执行鉴权、计费预扣、pre-call、选择、改写、转发、
// 结算与日志。这是所有转发面端点的唯一编排入口，协议差异全部收敛在 proto 实现里。
func (g *Gateway) Serve(c *gin.Context, proto relay.Protocol) {
	start := time.Now()

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

	// ---- 1.5 令牌级 IP 白名单（M2-wave1）：非空时客户端 IP 必须命中；
	// 鉴权后立即拒绝，不读请求体。客户端 IP 无法解析时从严拒绝。
	if !tokenAllowsIP(tok, c.ClientIP()) {
		log.Printf("[relay] 令牌 %d IP 白名单拒绝: %s", tok.ID, c.ClientIP())
		proto.WriteError(c, http.StatusForbidden, "token_ip_forbidden", "客户端 IP 不在令牌白名单内")
		return
	}

	// ---- 1.6 令牌预算周期惰性重置（M2-wave3）：窗口过期则滚动边界并复原
	// remain_quota（CAS 保证并发下恰一次），再进入限流与计费预扣；无周期/
	// unlimited 令牌零成本透传。
	tok = g.rollBudget(tok)

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

	// ---- 3.5 令牌级模型白名单（M2-wave1）：非空时请求模型必须命中。
	if !tokenAllowsModel(tok, pr.Model) {
		log.Printf("[relay] 令牌 %d 模型白名单拒绝: %s", tok.ID, pr.Model)
		proto.WriteError(c, http.StatusForbidden, "token_model_forbidden", "模型不在令牌可用清单内")
		return
	}

	// ---- 3.6 限流（M2-wave1）：令牌级 TPM/RPM + 全局/分组请求限流，
	// 超限 429 + Retry-After。限流在计费预扣之前：被拒请求无计费副作用；
	// 本机限流发生在渠道选择之前，不计入上游熔断（避免把自家限流算进上游健康度）。
	tokLim, _ := parseTPMRPM(tok.TPMRPM)
	if tokLim.TPM > 0 || tokLim.RPM > 0 {
		if allowed, retry := g.rl.AllowTokens(tokenLimitKey(tok.ID), tokLim.TPM, tokLim.RPM); !allowed {
			log.Printf("[ratelimit] 令牌 %d TPM/RPM 超限 tpm=%d rpm=%d", tok.ID, tokLim.TPM, tokLim.RPM)
			writeRateLimited(c, proto, retry)
			return
		}
	}
	reqCfg := g.requestLimitConfig(tokenGroup(tok))
	reqLimitKey := ""
	if reqCfg.Enabled {
		reqLimitKey = reqCfg.Scope + "|" + c.ClientIP()
		if allowed, retry := g.rl.AllowRequest(reqLimitKey, reqCfg.Window, reqCfg.MaxRequests, reqCfg.MaxSuccess); !allowed {
			log.Printf("[ratelimit] 请求限流超限 scope=%s window=%s", reqCfg.Scope, reqCfg.Window)
			writeRateLimited(c, proto, retry)
			return
		}
	}

	// ---- 4. 计费预检：解析价格快照；未配价显式拒绝（docs/04 第五节）。
	price, err := g.price.LookupPrice(pr.Model)
	if err != nil {
		if errors.Is(err, billing.ErrUnpriced) {
			log.Printf("[billing] 未配价模型拒绝服务: %v", err)
			proto.WriteError(c, http.StatusServiceUnavailable, "model_not_priced", "模型暂未配置价格")
		} else {
			log.Printf("[billing] 价格解析失败: %v", err)
			proto.WriteError(c, http.StatusServiceUnavailable, "model_not_priced", "模型价格配置不可用")
		}
		return
	}

	// ---- 5. 预扣冻结：估算上浮 20%，事务内条件扣减防透支（docs/04 第三节）。
	// unlimited 令牌跳过账本（frozen=0）。价格快照全请求复用，热更不影响在途请求口径。
	// 令牌分组（M2-wave1）：tokens.group → GroupRatio 组倍率与分组限流归属（缺省 default）。
	group := tokenGroup(tok)
	frozen := int64(0)
	if !tok.UnlimitedQuota {
		frozen = g.price.Estimate(price, group, raw, extractMaxTokens(raw))
		if err := g.ledger.Freeze(tok.ID, tok.UserID, frozen); err != nil {
			if errors.Is(err, billing.ErrInsufficientQuota) {
				proto.WriteError(c, http.StatusForbidden, "insufficient_quota", "令牌余额不足")
			} else {
				log.Printf("[billing] 预扣冻结失败 token=%d: %v", tok.ID, err)
				proto.WriteError(c, http.StatusInternalServerError, "gateway_error", "计费冻结失败")
			}
			return
		}
	}

	// ---- 6. 结算兜底：任何未走显式结算的退出路径（重试穷尽 / 无渠道 / panic 除外）
	// 全额退款并记 aborted 日志。settled 标志保证退款与日志恰好一次。
	settled := false
	detail := billing.Detail{
		Mode:      string(price.Mode),
		Expr:      price.Expr,
		Frozen:    frozen,
		Unlimited: tok.UnlimitedQuota,
	}
	if price.Mode == billing.ModeClassicRatio {
		detail.ModelRatio = price.ModelRatio
		detail.CompRatio = price.CompletionRatio
	}
	logDone := false
	submitLog := func(prompt, completion, quota int, d billing.Detail) {
		if logDone {
			return
		}
		logDone = true
		g.logs.Submit(billing.LogRecord{
			UserID:           tok.UserID,
			TokenID:          tok.ID,
			Protocol:         proto.Name(),
			ModelName:        pr.Model,
			PromptTokens:     prompt,
			CompletionTokens: completion,
			Quota:            int64(quota),
			UseTime:          int64(time.Since(start).Seconds()),
			IsStream:         pr.Stream,
			CreatedTime:      start.Unix(),
			Detail:           d,
		})
	}
	defer func() {
		if frozen > 0 && !settled {
			if err := g.ledger.RefundFull(tok.ID, tok.UserID, frozen); err != nil {
				log.Printf("[billing] 全额退款失败 token=%d frozen=%d: %v", tok.ID, frozen, err)
			} else {
				log.Printf("[relay] 请求未完成，已全额退款 model=%s token=%d frozen=%d", pr.Model, tok.ID, frozen)
			}
			detail.Aborted = true
			detail.RefundFull = true
		}
		submitLog(0, 0, 0, detail)
	}()

	// ---- 7. 重试循环：typed retry，重试仅限首字节前（2xx 交给 Respond 后不再重试）。
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

		// ---- 8. 2xx：协议适配层转发（流式逐事件 flush / 非流式透传）+ 计费结算。
		// 从此处起首字节即将写出，任何失败都不再重试。
		g.breaker.OnSuccess(ch.ID)
		usage, respErr := proto.Respond(c, resp, pr)
		if respErr != nil {
			log.Printf("[relay] 渠道 %d 响应转发失败 model=%s: %v", ch.ID, pr.Model, respErr)
		}
		// 限流记账（M2-wave1）：成功完成计入全局/分组成功数窗口；
		// 实际用量计入令牌 TPM 窗口（流中断不计成功，用量照记——已发生即已消耗）。
		if respErr == nil && reqLimitKey != "" {
			g.rl.RecordSuccess(reqLimitKey, reqCfg.Window)
		}
		if tokLim.TPM > 0 {
			g.rl.RecordTokenUsage(tokenLimitKey(tok.ID), usage.PromptTokens+usage.CompletionTokens)
		}
		actual, d := g.settle(tok, price, group, frozen, raw, c, usage, respErr)
		detail = d
		settled = true
		submitLog(logPromptTokens(usage, detail.Estimated, raw),
			logCompletionTokens(usage, detail.Estimated, c), int(actual), detail)

		log.Printf("[relay] %s model=%s channel=%d token=%d stream=%v prompt=%d completion=%d quota=%d frozen=%d",
			proto.Name(), pr.Model, ch.ID, tok.ID, pr.Stream,
			usage.PromptTokens, usage.CompletionTokens, actual, frozen)
		return
	}
}

// settle 是 Respond 之后的统一结算：成功按实际 usage 多退少补；usage 缺失但有
// 正常内容 → 本地粗估并标记 estimated；转发失败/流中断 → 全额退款并标记 aborted
// （docs/04 第三、四节）。返回实结 quota 与落库明细。
func (g *Gateway) settle(tok *model.Token, price *billing.ModelPrice, group string,
	frozen int64, raw []byte, c *gin.Context, usage relay.Usage, respErr error) (int64, billing.Detail) {

	detail := billing.Detail{
		Mode:       string(price.Mode),
		Expr:       price.Expr,
		Frozen:     frozen,
		Unlimited:  tok.UnlimitedQuota,
		GroupRatio: g.price.GroupRatio(group),
	}
	if price.Mode == billing.ModeClassicRatio {
		detail.ModelRatio = price.ModelRatio
		detail.CompRatio = price.CompletionRatio
	}
	detail.CacheRead = min(max(usage.CacheReadTokens, 0), usage.PromptTokens)
	detail.BilledIn = usage.PromptTokens - detail.CacheRead

	// ---- 流中断 / 转发失败：全额退款（宁可少收不多收）。
	if respErr != nil {
		if frozen > 0 {
			if err := g.ledger.RefundFull(tok.ID, tok.UserID, frozen); err != nil {
				log.Printf("[billing] 流中断退款失败 token=%d frozen=%d: %v", tok.ID, frozen, err)
			}
		}
		detail.Aborted = true
		detail.RefundFull = frozen > 0
		return 0, detail
	}

	// ---- 结算计费。
	var actual int64
	var err error
	switch {
	case usage.PromptTokens == 0 && usage.CompletionTokens == 0:
		// usage 缺失但有正常内容：本地粗估（输入按请求体、输出按已写字节，4B/token）
		// 并标记 estimated。粗估口径偏保守（缓存折扣不可知，按全价输入计）。
		est := billing.Usage{
			Input:      len(raw) / billing.BytesPerTokenEstimate,
			Completion: c.Writer.Size() / billing.BytesPerTokenEstimate,
		}
		actual, err = g.price.Charge(price, group, est)
		detail.Estimated = true
		detail.BilledIn = est.Input
	default:
		actual, err = g.price.Charge(price, group, billing.Usage{
			Input:      usage.PromptTokens,
			Completion: usage.CompletionTokens,
			CacheRead:  usage.CacheReadTokens,
		})
	}
	if err != nil {
		// 计费求值失败（理论不可达：Lookup 已校验表达式）。全额退款，宁可少收。
		log.Printf("[billing] 结算计费失败(模型 %s): %v", price.Model, err)
		if frozen > 0 {
			if err := g.ledger.RefundFull(tok.ID, tok.UserID, frozen); err != nil {
				log.Printf("[billing] 结算失败退款异常 token=%d: %v", tok.ID, err)
			}
		}
		detail.Aborted = true
		detail.RefundFull = frozen > 0
		detail.Err = "settle_charge_failed"
		return 0, detail
	}

	// ---- 多退少补（unlimited 令牌跳过账本：冻结与补扣均不记账，日志仍记实结 quota）。
	if !tok.UnlimitedQuota {
		if err := g.ledger.Settle(tok.ID, tok.UserID, frozen, actual); err != nil {
			log.Printf("[billing] 多退少补失败 token=%d frozen=%d actual=%d: %v", tok.ID, frozen, actual, err)
		}
	}
	return actual, detail
}

// logPromptTokens 返回日志应记录的输入 tokens：estimated 粗估值，否则上游原值。
func logPromptTokens(usage relay.Usage, estimated bool, raw []byte) int {
	if estimated {
		return len(raw) / billing.BytesPerTokenEstimate
	}
	return usage.PromptTokens
}

// logCompletionTokens 返回日志应记录的输出 tokens：estimated 按已写字节粗估，否则上游原值。
func logCompletionTokens(usage relay.Usage, estimated bool, c *gin.Context) int {
	if estimated {
		return c.Writer.Size() / billing.BytesPerTokenEstimate
	}
	return usage.CompletionTokens
}

// extractMaxTokens 从请求体提取 max_tokens（OpenAI/Anthropic 同名字段）；
// 缺失或非法时返回 0（由 Estimate 取缺省估算值）。
func extractMaxTokens(raw []byte) int {
	var probe struct {
		MaxTokens int `json:"max_tokens"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.MaxTokens
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
