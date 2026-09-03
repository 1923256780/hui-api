// 变换层：旧库源行 → hui 模型行的纯函数集合（无 IO，全部可单测）。
//
// 设计约束（docs/07、ADR-0008）：
//   - 变换规则全部显式：未知类型/非法数据一律硬失败（返回 error），由引擎层记入
//     报告并跳过该行，绝不静默丢弃或猜测；
//   - key_hash 必须复用 internal/gateway 的 HashKey，防止迁移口径与运行时鉴权口径漂移；
//   - ModelRatio 换算方向为 legacy×2（推导与黄金锚定见 ADR-0008 与 transform_test.go）。
package migrate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/override"
)

// 与迁移口径相关的固定常量。
const (
	// optionKeyQuotaPerUnit 是旧库 options 中的额度单位键；缺省（未落库）即 500000。
	optionKeyQuotaPerUnit = "QuotaPerUnit"

	// TokenPlainPrefix 是 hui 令牌明文前缀：迁移时补齐为 "sk-"+旧库 48 位明文。
	TokenPlainPrefix = "sk-"

	// ModelRatioConversionFactor 是 ModelRatio 的 legacy→hui 换算系数。
	// 旧网关 classic 计费：quota = (p + c×cr) × mr_legacy × gr（无 /1e6 环节，
	// 其 mr 数值已内嵌比例）；hui 计费：quota = (p + c×cr) × mr_hui × gr × 500000/1e6。
	// 令两式对同一账单相等 ⇒ mr_hui = mr_legacy × 2。黄金锚定见 transform_test.go
	//（TestModelRatioGoldenAnchoring：deepseek-v4-flash 旧账 85 / 新算 85）。
	ModelRatioConversionFactor = 2.0

	// modelRatioConversionNote 是报告中 ModelRatio 换算口径说明（固定文本，保证报告确定性）。
	modelRatioConversionNote = "legacy×2"

	// detailMaxBytes 是 hui logs.detail 列容量（size:2048）；超长的旧 other 置空。
	detailMaxBytes = 2048

	// 旧库日志协议口径：旧库无协议列，消费日志统一按 OpenAI 兼容口径迁移。
	logProtocolConsume = "openai"
	logProtocolTopup   = "topup"
	topupModelName     = "redemption"
)

// 旧库渠道类型（仅迁移涉及的原生类型；其余类型硬失败人工复核）。
const (
	legacyTypeOpenAI     = 1  // OpenAI 兼容
	legacyTypeArk        = 14 // 火山方舟
	legacyTypeOpenRouter = 20 // OpenRouter
	legacyTypeDeepSeek   = 43 // DeepSeek
)

// migrateOptionKeys 是 options 白名单（逐字迁移的 4 键 + 换算迁移的 ModelRatio）。
// 键名引用 billing 常量防口径漂移。
var migrateOptionKeys = []string{
	billing.OptionKeyBillingMode,
	billing.OptionKeyBillingExpr,
	billing.OptionKeyGroupRatio,
	billing.OptionKeyCompletionRatio,
	billing.OptionKeyModelRatio,
}

// legacyTypeBaseURLFallback 是空 base_url 的官方端点映射（均属上游模型服务商公开端点）。
var legacyTypeBaseURLFallback = map[int]string{
	legacyTypeDeepSeek:   "https://api.deepseek.com",
	legacyTypeOpenRouter: "https://openrouter.ai/api/v1",
	legacyTypeArk:        "https://ark.cn-beijing.volces.com/api/v1",
}

// ---- users ----

// TransformUser 迁移用户行。守卫：密码哈希必须带 bcrypt $2a$ 前缀（否则硬失败）、
// status 必须是 hui 合法枚举（1 启用 / 2 禁用）。group 空 → default；
// aff 相关列（AffCode/InviterID/AffHistoryQuota）与会话列（AuthVersion/TOTP*）不迁移。
func TransformUser(u legacyUser) (model.User, error) {
	// bcrypt 守卫：标准 bcrypt 哈希恒为 60 字符（$2a$<cost>$ + 53 位哈希体），
	// 防止空串/截断/明文密码混入后无法登录。
	if !strings.HasPrefix(u.Password, "$2a$") || len(u.Password) != 60 {
		return model.User{}, fmt.Errorf("用户 %q(id=%d) 密码哈希非合法 bcrypt 格式，拒绝迁移", u.Username, u.ID)
	}
	switch u.Status {
	case model.StatusEnabled, model.StatusDisabled:
	default:
		return model.User{}, fmt.Errorf("用户 %q(id=%d) 状态 %d 不在 hui 用户状态枚举内，拒绝迁移", u.Username, u.ID, u.Status)
	}
	group := billing.DefaultGroup
	if g := strings.TrimSpace(ptrStr(u.Group)); g != "" {
		group = g
	}
	return model.User{
		ID:            u.ID,
		Username:      u.Username,
		PasswordHash:  u.Password,
		DisplayName:   ptrStr(u.DisplayName),
		Role:          u.Role,
		Status:        u.Status,
		Quota:         u.Quota,
		UsedQuota:     u.UsedQuota,
		Email:         ptrStr(u.Email),
		Group:         group,
		CreatedTime:   ptrI64(u.CreatedAt),
		LastLoginTime: ptrI64(u.LastLoginAt),
	}, nil
}

// ---- tokens ----

// TransformToken 迁移令牌行。key="sk-"+旧明文；key_hash 必须经 gateway.HashKey 计算
// （与运行时鉴权唯一口径）；group 空继承所属用户组；unlimited 归一化为 quota=0。
// 非无限令牌的预算快照：quota=remain=旧 remain_quota（无周期，BudgetDuration 留空）。
// 旧状态映射：2 手动禁用 → hui 禁用；1/3/4（启用/过期/耗尽）→ hui 启用，
// 过期与耗尽语义由 hui 行级校验（expired_time / remain_quota）兜底；其他值硬失败。
func TransformToken(t legacyToken, userGroup string) (model.Token, error) {
	if strings.TrimSpace(t.Key) == "" {
		return model.Token{}, fmt.Errorf("令牌 id=%d key 为空，拒绝迁移", t.ID)
	}
	plain := TokenPlainPrefix + t.Key
	status, err := mapTokenStatus(t.Status)
	if err != nil {
		return model.Token{}, fmt.Errorf("令牌 id=%d: %w", t.ID, err)
	}
	group := strings.TrimSpace(userGroup)
	if group == "" {
		group = billing.DefaultGroup
	}
	if g := strings.TrimSpace(ptrStr(t.Group)); g != "" {
		group = g
	}
	unlimited := t.UnlimitedQuota != 0
	quota, remain := int64(0), int64(0)
	if !unlimited {
		quota = ptrI64(t.RemainQuota)
		remain = quota
	}
	limits := ""
	if t.ModelLimitsEnabled != nil && *t.ModelLimitsEnabled != 0 {
		limits = strings.TrimSpace(ptrStr(t.ModelLimits))
	}
	return model.Token{
		ID:             t.ID,
		UserID:         t.UserID,
		Name:           ptrStr(t.Name),
		Key:            plain,
		KeyHash:        gateway.HashKey(plain),
		Status:         status,
		Quota:          quota,
		RemainQuota:    remain,
		UnlimitedQuota: unlimited,
		ModelLimits:    limits,
		AllowIPs:       ptrStr(t.AllowIPs),
		Group:          group,
		ExpiredTime:    normalizeEpoch(ptrI64(t.ExpiredTime)),
		CreatedTime:    ptrI64(t.CreatedTime),
		AccessedTime:   ptrI64(t.AccessedTime),
	}, nil
}

// mapTokenStatus 映射旧令牌状态。
func mapTokenStatus(s int) (int, error) {
	switch s {
	case 2:
		return model.StatusDisabled, nil
	case 1, 3, 4:
		return model.StatusEnabled, nil
	default:
		return 0, fmt.Errorf("状态 %d 不在旧令牌状态枚举内", s)
	}
}

// normalizeEpoch 归一化过期时间：≤0 一律 EpochForever（旧库 -1/0 均表示不过期语义）。
func normalizeEpoch(v int64) int64 {
	if v <= 0 {
		return model.EpochForever
	}
	return v
}

// ---- channels ----

// ChannelTransformResult 是渠道行变换结果：hui 渠道行 + 特有缺口信息。
type ChannelTransformResult struct {
	Channel         model.Channel
	ModelMappingRaw string // 非空表示旧 model_mapping 未迁移（缺口，原文摘要入报告）
	GroupDropped    bool   // 旧多组信息（group 列）在 hui 无对应列而丢弃
}

// TransformChannel 迁移渠道行。
//   - type：1/14/20/43 → hui 1（OpenAI 兼容）；其他类型硬失败；
//   - status：1 启用 → 1；2 手动禁用 / 3 自动禁用 → 2（禁用，人工复核后启用）；其他硬失败；
//   - base_url 空：按类型补官方端点（DeepSeek/OpenRouter/方舟）；无映射可用则硬失败；
//   - param_override：envelope+operations 高级格式 → 扁平 ops（TransformParamOverride），
//     非 envelope 格式硬失败该行；
//   - model_mapping 不迁移（缺口入报告）；group 多组信息丢弃（计数入报告）。
func TransformChannel(c legacyChannel) (ChannelTransformResult, error) {
	typ, err := mapChannelType(c.Type)
	if err != nil {
		return ChannelTransformResult{}, fmt.Errorf("渠道 id=%d: %w", c.ID, err)
	}
	status, err := mapChannelStatus(c.Status)
	if err != nil {
		return ChannelTransformResult{}, fmt.Errorf("渠道 id=%d: %w", c.ID, err)
	}
	baseURL := strings.TrimSpace(ptrStr(c.BaseURL))
	if baseURL == "" {
		fb, ok := legacyTypeBaseURLFallback[c.Type]
		if !ok {
			return ChannelTransformResult{}, fmt.Errorf("渠道 id=%d type=%d base_url 为空且无已知官方端点映射，拒绝迁移", c.ID, c.Type)
		}
		baseURL = fb
	}
	po := ""
	if raw := strings.TrimSpace(ptrStr(c.ParamOverride)); raw != "" {
		po, err = TransformParamOverride(raw)
		if err != nil {
			return ChannelTransformResult{}, fmt.Errorf("渠道 id=%d param_override: %w", c.ID, err)
		}
	}
	return ChannelTransformResult{
		Channel: model.Channel{
			ID:            c.ID,
			Name:          ptrStr(c.Name),
			Type:          typ,
			BaseURL:       baseURL,
			Key:           c.Key,
			Models:        ptrStr(c.Models),
			Priority:      c.Priority,
			Weight:        c.Weight,
			Status:        status,
			ParamOverride: po,
			CreatedTime:   ptrI64(c.CreatedTime),
		},
		ModelMappingRaw: oneLine(ptrStr(c.ModelMapping), modelMappingSummaryMax),
		GroupDropped:    strings.TrimSpace(ptrStr(c.Group)) != "",
	}, nil
}

// mapChannelType 映射旧渠道类型到 hui（当前仅 OpenAI 兼容协议）。
func mapChannelType(t int) (int, error) {
	switch t {
	case legacyTypeOpenAI, legacyTypeArk, legacyTypeOpenRouter, legacyTypeDeepSeek:
		return model.ChannelTypeOpenAICompatible, nil
	default:
		return 0, fmt.Errorf("类型 %d 无映射规则，人工复核", t)
	}
}

// mapChannelStatus 映射旧渠道状态（hui 3=熔断由运行时产生，迁移不落熔断态）。
func mapChannelStatus(s int) (int, error) {
	switch s {
	case 1:
		return model.StatusEnabled, nil
	case 2, 3:
		return model.StatusDisabled, nil
	default:
		return 0, fmt.Errorf("状态 %d 不在旧渠道状态枚举内", s)
	}
}

// ---- param_override：envelope → 扁平 ops ----

// legacyOverrideEnvelope 是旧库 channels.param_override 的高级模式封装格式。
// Operations 用指针区分“缺字段”（硬失败）与“空数组”（合法空操作集）。
type legacyOverrideEnvelope struct {
	Mode       string              `json:"mode"`
	Operations *[]legacyOverrideOp `json:"operations"`
}

// legacyOverrideOp 是旧 envelope 内的单个操作（按数组顺序执行）。
type legacyOverrideOp struct {
	Mode        string          `json:"mode"`
	Path        string          `json:"path"`
	Value       json.RawMessage `json:"value"`       // set/append 的任意 JSON 值
	Old         string          `json:"old"`         // replace
	New         string          `json:"new"`         // replace
	Pattern     string          `json:"pattern"`     // regex_replace
	Replacement string          `json:"replacement"` // regex_replace
}

// TransformParamOverride 把旧 envelope+operations 高级格式变换为 hui override 管道的
// 扁平 ops JSON（{"delete":[...],"set":{...},...}）。空输入返回空串。
// 非 envelope 格式、未知操作 mode、非法正则一律硬失败。
// 产物经 override.Parse 反解析自校验，保证 hui 运行时管道可解析。
func TransformParamOverride(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	var env legacyOverrideEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return "", fmt.Errorf("非 envelope JSON: %w", err)
	}
	if env.Mode != "advanced" {
		return "", fmt.Errorf("mode=%q 非 advanced envelope 格式", env.Mode)
	}
	if env.Operations == nil {
		return "", fmt.Errorf("advanced envelope 缺少 operations 字段")
	}
	ops := &override.Ops{
		Set:          map[string]any{},
		Append:       map[string]any{},
		Replace:      map[string]override.ReplaceOp{},
		RegexReplace: map[string]override.RegexOp{},
	}
	for i, op := range *env.Operations {
		switch op.Mode {
		case "delete":
			if strings.TrimSpace(op.Path) == "" {
				return "", fmt.Errorf("operations[%d] delete 缺少 path", i)
			}
			ops.Delete = append(ops.Delete, op.Path)
		case "set", "append":
			if strings.TrimSpace(op.Path) == "" {
				return "", fmt.Errorf("operations[%d] %s 缺少 path", i, op.Mode)
			}
			if len(op.Value) == 0 {
				return "", fmt.Errorf("operations[%d] %s 缺少 value", i, op.Mode)
			}
			var v any
			if err := json.Unmarshal(op.Value, &v); err != nil {
				return "", fmt.Errorf("operations[%d] %s value 非法: %w", i, op.Mode, err)
			}
			if op.Mode == "set" {
				ops.Set[op.Path] = v
			} else {
				ops.Append[op.Path] = v
			}
		case "replace":
			if strings.TrimSpace(op.Path) == "" {
				return "", fmt.Errorf("operations[%d] replace 缺少 path", i)
			}
			ops.Replace[op.Path] = override.ReplaceOp{Old: op.Old, New: op.New}
		case "regex_replace":
			if strings.TrimSpace(op.Path) == "" {
				return "", fmt.Errorf("operations[%d] regex_replace 缺少 path", i)
			}
			if _, err := regexp.Compile(op.Pattern); err != nil {
				return "", fmt.Errorf("operations[%d] regex_replace 正则 %q 编译失败: %w", i, op.Pattern, err)
			}
			ops.RegexReplace[op.Path] = override.RegexOp{Pattern: op.Pattern, Replacement: op.Replacement}
		default:
			return "", fmt.Errorf("operations[%d] 未知 mode %q", i, op.Mode)
		}
	}
	out, err := json.Marshal(ops)
	if err != nil {
		return "", fmt.Errorf("序列化扁平 ops: %w", err)
	}
	// 自校验：产物必须能被 hui override 管道原样解析（防两端口径漂移）。
	if _, err := override.Parse(string(out)); err != nil {
		return "", fmt.Errorf("扁平 ops 自校验失败: %w", err)
	}
	return string(out), nil
}

// ---- redemptions ----

// TransformRedemption 迁移兑换码行。状态映射（旧枚举：1 未用 / 2 禁用 / 3 已核销）：
// 3 已核销 → hui 2；2 禁用 → hui 3 作废；1 未用且已过期（expired_time < now）→ hui 4，
// 否则 hui 1；其他值硬失败。expired_time ≤ 0 → EpochForever。
func TransformRedemption(r legacyRedemption, nowUnix int64) (model.Redemption, error) {
	status := 0
	switch r.Status {
	case 3:
		status = model.RedemptionRedeemed
	case 2:
		status = model.RedemptionVoided
	case 1:
		if exp := ptrI64(r.ExpiredTime); exp > 0 && exp < nowUnix {
			status = model.RedemptionExpired
		} else {
			status = model.RedemptionUnused
		}
	default:
		return model.Redemption{}, fmt.Errorf("兑换码 id=%d 状态 %d 无映射规则", r.ID, r.Status)
	}
	return model.Redemption{
		ID:          r.ID,
		Key:         r.Key,
		Name:        ptrStr(r.Name),
		Status:      status,
		Quota:       r.Quota,
		CreatedBy:   ptrI64(r.UserID),
		UsedBy:      ptrI64(r.UsedUserID),
		UsedTime:    ptrI64(r.RedeemedTime),
		ExpiredTime: normalizeEpoch(ptrI64(r.ExpiredTime)),
		CreatedTime: ptrI64(r.CreatedTime),
	}, nil
}

// ---- logs ----

// LogDetailOutcome 是消费日志 detail（旧 other 字段）的处置结果。
type LogDetailOutcome int

const (
	DetailAbsent  LogDetailOutcome = iota // 旧 other 为空：无 detail
	DetailKept                            // 合法 JSON 且未超长：保留为 detail
	DetailDropped                         // 非法 JSON 或超长：置空
)

// TransformConsumeLog 迁移消费日志（旧 type=2）。旧库无协议列，统一按
// OpenAI 兼容口径（protocol="openai"）；旧 other 计费依据 JSON 保留为 detail
// （合法 JSON 且 ≤2048 字节），否则置空。content/username/ip 等列 hui 无对应列，不迁移。
func TransformConsumeLog(l legacyLog) (model.Log, LogDetailOutcome, error) {
	detail, outcome, err := transformDetail(l.Other)
	if err != nil {
		return model.Log{}, DetailAbsent, fmt.Errorf("日志 id=%d: %w", l.ID, err)
	}
	return model.Log{
		ID:               l.ID,
		UserID:           l.UserID,
		TokenID:          ptrI64(l.TokenID),
		ChannelID:        l.ChannelID,
		Protocol:         logProtocolConsume,
		ModelName:        ptrStr(l.ModelName),
		PromptTokens:     int(l.PromptTokens),
		CompletionTokens: int(l.CompletionTokens),
		Quota:            l.Quota,
		UseTime:          l.UseTime,
		IsStream:         l.IsStream != 0,
		Detail:           detail,
		CreatedTime:      l.CreatedAt,
	}, outcome, nil
}

// transformDetail 处置旧 other 字段。
func transformDetail(other *string) (string, LogDetailOutcome, error) {
	if other == nil || strings.TrimSpace(*other) == "" {
		return "", DetailAbsent, nil
	}
	if len(*other) > detailMaxBytes {
		return "", DetailDropped, nil
	}
	if !json.Valid([]byte(*other)) {
		return "", DetailDropped, nil
	}
	return *other, DetailKept, nil
}

// redemptionIDRefPattern 从旧充值日志 content 提取兑换码 ID（"……兑换码ID N"）。
var redemptionIDRefPattern = regexp.MustCompile(`兑换码ID\s*(\d+)`)

// TransformTopupLog 把旧 type=1 充值日志合成为 hui topup 对账日志
// （形状对齐 internal/api 的 topup 日志：protocol=topup、model=redemption、detail 记来源）。
// 面值优先从 content 的"兑换码ID N"关联旧 redemptions.quota；关联成功返回 linked=true。
func TransformTopupLog(l legacyLog, redemptionQuota map[int64]int64) (model.Log, bool, error) {
	quota := int64(0)
	ref := fmt.Sprintf("legacy-log-%d", l.ID)
	linked := false
	if m := redemptionIDRefPattern.FindStringSubmatch(ptrStr(l.Content)); m != nil {
		var id int64
		if _, err := fmt.Sscanf(m[1], "%d", &id); err == nil {
			if q, ok := redemptionQuota[id]; ok {
				quota = q
				ref = fmt.Sprintf("legacy-redemption-%d", id)
				linked = true
			}
		}
	}
	detail, err := json.Marshal(map[string]any{"event": "topup", "ref_id": ref, "quota": quota})
	if err != nil {
		return model.Log{}, false, fmt.Errorf("日志 id=%d: %w", l.ID, err)
	}
	return model.Log{
		ID:          l.ID,
		UserID:      l.UserID,
		Protocol:    logProtocolTopup,
		ModelName:   topupModelName,
		Quota:       quota,
		Detail:      string(detail),
		CreatedTime: l.CreatedAt,
	}, linked, nil
}

// ---- options / ModelRatio ----

// ModelRatioTransformResult 是 ModelRatio 换算结果。
type ModelRatioTransformResult struct {
	MigratedJSON string   // 换算后的 JSON（键升序输出）
	Migrated     []string // 最小集内成功换算的模型（升序）
	Missing      []string // 最小集内旧库无价的模型（升序，缺口）
}

// TransformModelRatio 把旧 ModelRatio JSON 按启用渠道模型最小集过滤并按
// legacy×2 换算为 hui 口径。最小集内旧库无价的模型记入 Missing（缺口），
// 不阻断迁移（tiered_expr 模型无需 ModelRatio，由报告提示人工确认）。
// 非 JSON 输入硬失败（源价格单损坏宁可拒绝迁移）。
func TransformModelRatio(rawJSON string, activeModels []string) (ModelRatioTransformResult, error) {
	var legacy map[string]float64
	if strings.TrimSpace(rawJSON) != "" {
		if err := json.Unmarshal([]byte(rawJSON), &legacy); err != nil {
			return ModelRatioTransformResult{}, fmt.Errorf("旧库 ModelRatio JSON 解析失败: %w", err)
		}
	}
	uniq := make([]string, 0, len(activeModels))
	seen := make(map[string]struct{}, len(activeModels))
	for _, m := range activeModels {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	sort.Strings(uniq)

	out := make(map[string]float64, len(uniq))
	res := ModelRatioTransformResult{Migrated: []string{}, Missing: []string{}}
	for _, m := range uniq {
		v, ok := legacy[m]
		if !ok {
			res.Missing = append(res.Missing, m)
			continue
		}
		out[m] = v * ModelRatioConversionFactor
		res.Migrated = append(res.Migrated, m)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ModelRatioTransformResult{}, fmt.Errorf("序列化换算后 ModelRatio: %w", err)
	}
	res.MigratedJSON = string(b)
	return res, nil
}

// ActiveModelsFromChannels 从 hui 渠道行收集启用渠道的模型并集（去重升序）：
// ModelRatio 换算的最小集依据——价格单只覆盖实际可路由的模型。
func ActiveModelsFromChannels(chs []model.Channel) []string {
	set := make(map[string]struct{})
	for _, ch := range chs {
		if ch.Status != model.StatusEnabled {
			continue
		}
		for _, m := range strings.Split(ch.Models, ",") {
			if s := strings.TrimSpace(m); s != "" {
				set[s] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ---- 小工具 ----

// ptrStr 安全解引用字符串指针。
func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ptrI64 安全解引用整数指针。
func ptrI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
