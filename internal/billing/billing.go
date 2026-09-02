// Package billing 是 hui-api 的计费内核（docs/01 设计点 4、docs/04）：
// 三模式计费（classic_ratio / tiered_expr / per_call）、价格配置解析
// （options 运行轨优先、内置 prices.json 兜底）、预扣费账本（冻结 / 多退少补 /
// 全额退款）与请求日志异步批量落库。
//
// 计费是可信底线：宁可拒绝请求，不可错扣一分——未配价模型显式拒绝服务，
// 结算失败按全额退款处理（宁可少收不多收）。
//
// quota 单位约定（docs/03）：整数计费单位，500000 quota = $1（model.QuotaPerDollar）。
// 价格配置以「每百万 tokens 的美元单价」表达，中间过程允许浮点，产出必须四舍五入为 quota 整数。
package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync/atomic"

	"github.com/1923256780/hui-api/internal/model"
)

// Mode 是计费模式（docs/04 第二节）。
type Mode string

// 三种计费模式。
const (
	ModeClassicRatio Mode = "classic_ratio" // 倍率模式：输入/输出倍率线性计价
	ModeTieredExpr   Mode = "tiered_expr"   // 表达式模式：每模型计费表达式（支持 tier()）
	ModePerCall      Mode = "per_call"      // 按次模式：固定价格/次
)

// options 运行轨的计费配置键（docs/04：模式声明与配置变更均走 options 热更）。
const (
	// OptionKeyBillingMode 模型名 → 模式声明 JSON，如 {"glm-x":"tiered_expr"}。
	OptionKeyBillingMode = "billing_setting.billing_mode"
	// OptionKeyBillingExpr 模型名 → 计费表达式 JSON（tiered_expr 模式用），
	// 如 {"glm-x":"tier(\"base\", p * 0.15 + c * 0.5 + cr * 0.014)"}。
	OptionKeyBillingExpr = "billing_setting.billing_expr"
	// OptionKeyBillingPrice 模型名 → 每次美元单价 JSON（per_call 模式用）。
	OptionKeyBillingPrice = "billing_setting.billing_price"
	// OptionKeyModelRatio 模型名 → 每百万输入 tokens 美元单价 JSON（classic 回退价）。
	OptionKeyModelRatio = "ModelRatio"
	// OptionKeyCompletionRatio 模型名 → 输出价相对输入价的倍数 JSON（classic 回退价）。
	OptionKeyCompletionRatio = "CompletionRatio"
	// OptionKeyGroupRatio 组名 → 组倍率 JSON；缺省组 "default" 恒为 1.0。
	OptionKeyGroupRatio = "GroupRatio"
)

// DefaultGroup 是本波令牌分组语义：所有请求归属 default 组（M3 令牌分组落地前）。
const DefaultGroup = "default"

// ErrUnpriced 表示模型无任何价格配置：显式拒绝服务（docs/04 第五节，映射 HTTP 503）。
var ErrUnpriced = errors.New("model is not priced")

// 计费估算参数（docs/04：预扣费 = max_tokens 与输入长度估算上浮 20%）。
const (
	// DefaultMaxTokensEstimate 请求未携带 max_tokens 时的估算输出长度。
	DefaultMaxTokensEstimate = 1024
	// BytesPerTokenEstimate 本地粗估：约 4 字节 ≈ 1 token。
	BytesPerTokenEstimate = 4
	// EstimateHeadroom 预扣上浮系数（1.2 = 上浮 20%）。
	EstimateHeadroom = 1.2
)

// Usage 是计费输入用量（与 relay.Usage 对齐）。
// CacheRead 是提示缓存读取部分，已计入 Input（PromptTokens）；
// 计费拆分时以 Input-CacheRead 与 CacheRead 分别作为表达式变量 p 与 cr，避免重复计费。
type Usage struct {
	Input      int // 输入 tokens（含缓存读取部分）
	Completion int // 输出 tokens
	CacheRead  int // 缓存读取 tokens（Input 的子集）
}

// ModelPrice 是单模型的计费价格快照：一次请求生命周期内复用同一份，
// 避免预扣与结算之间配置热更导致口径漂移。
type ModelPrice struct {
	Model           string
	Mode            Mode
	Expr            string  // tiered_expr：计费表达式
	ModelRatio      float64 // classic_ratio：每百万输入 tokens 美元单价
	CompletionRatio float64 // classic_ratio：输出价相对输入价的倍数（缺省 0，即输出不计费）
	PerCallPrice    float64 // per_call：每次美元单价
}

// Source 是价格配置源的最小接口（config.Runtime 天然满足；测试用内存 map 替身）。
type Source interface {
	Get(key string) (string, bool)
}

// Engine 是计费引擎：解析价格配置 + 三模式求值。
// 零值不可用，经 NewEngine 构造。并发安全。
type Engine struct {
	src Source // 运行轨配置源（可为 nil：仅用内置价）

	// settings 按 src 版本缓存的解析结果（options JSON 每次热更才重新解析）。
	settings atomic.Value // settingsCache
	// builtin 兜底价单（go:embed prices.json，启动校验 schema）。
	builtin map[string]ModelPrice
	// progCache 是表达式编译缓存：表达式文本 → 编译产物（并发安全）。
	progCache atomic.Value // *programCache
}

// NewEngine 构造计费引擎：加载并校验内置 prices.json（schema 非法时返回错误，
// 调用方应启动失败——价格单损坏时宁可拒绝全部计费请求）。
// src 允许为 nil（仅内置价，测试用）。
func NewEngine(src Source) (*Engine, error) {
	e := &Engine{src: src}

	builtin, err := loadBuiltinPrices()
	if err != nil {
		return nil, fmt.Errorf("内置价格单校验失败: %w", err)
	}
	e.builtin = builtin
	e.progCache.Store(newProgramCache())
	return e, nil
}

// GroupRatio 返回组倍率：GroupRatio options JSON 中对应组名；缺省 1.0。
// 本波所有请求归属 default 组（M3 令牌分组落地前）。
func (e *Engine) GroupRatio(group string) float64 {
	s := e.loadSettings()
	if v, ok := s.groupRatio[group]; ok && v >= 0 {
		return v
	}
	if v, ok := s.groupRatio[DefaultGroup]; ok && v >= 0 {
		return v
	}
	return 1.0
}

// LookupPrice 解析单模型价格快照。查找顺序（DB 配置优先于内置价）：
//
//  1. options billing_setting.billing_mode 显式声明模式（tiered_expr 配 billing_expr、
//     per_call 配 billing_price、classic_ratio 配 ModelRatio）；
//  2. options ModelRatio 存在该模型（隐式 classic_ratio）；
//  3. 内置 prices.json；
//  4. 均未命中 → ErrUnpriced。
//
// 配置声明不完整（如声明 tiered_expr 但无表达式）视为未配价：拒绝服务优于错扣。
func (e *Engine) LookupPrice(modelName string) (*ModelPrice, error) {
	if strings.TrimSpace(modelName) == "" {
		return nil, ErrUnpriced
	}
	s := e.loadSettings()

	if mode, ok := s.billingMode[modelName]; ok {
		switch Mode(mode) {
		case ModeTieredExpr:
			exprSrc, ok := s.billingExpr[modelName]
			if !ok || strings.TrimSpace(exprSrc) == "" {
				return nil, fmt.Errorf("%w: %s（tiered_expr 缺少表达式）", ErrUnpriced, modelName)
			}
			return &ModelPrice{Model: modelName, Mode: ModeTieredExpr, Expr: exprSrc}, nil
		case ModePerCall:
			price, ok := s.billingPrice[modelName]
			if !ok || price < 0 {
				return nil, fmt.Errorf("%w: %s（per_call 缺少单价）", ErrUnpriced, modelName)
			}
			return &ModelPrice{Model: modelName, Mode: ModePerCall, PerCallPrice: price}, nil
		case ModeClassicRatio:
			mr, ok := s.modelRatio[modelName]
			if !ok || mr < 0 {
				return nil, fmt.Errorf("%w: %s（classic_ratio 缺少 ModelRatio）", ErrUnpriced, modelName)
			}
			return &ModelPrice{Model: modelName, Mode: ModeClassicRatio,
				ModelRatio: mr, CompletionRatio: s.completionRatio[modelName]}, nil
		default:
			return nil, fmt.Errorf("%w: %s（未知计费模式 %q）", ErrUnpriced, modelName, mode)
		}
	}

	// 隐式 classic_ratio：ModelRatio 存在该模型即视为已配价（CompletionRatio 可缺省）。
	if mr, ok := s.modelRatio[modelName]; ok && mr >= 0 {
		return &ModelPrice{Model: modelName, Mode: ModeClassicRatio,
			ModelRatio: mr, CompletionRatio: s.completionRatio[modelName]}, nil
	}

	// 内置兜底价单。
	if p, ok := e.builtin[modelName]; ok {
		cp := p
		cp.Model = modelName
		return &cp, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnpriced, modelName)
}

// Charge 按价格快照与用量计费，返回四舍五入的 quota 整数。
//
//   - classic_ratio：(input + completion×completion_ratio) × model_ratio × group_ratio
//     × QuotaPerDollar / 1e6；
//   - tiered_expr：表达式变量 p=input-cache_read、c=completion、cr=cache_read，
//     单位均为原始 tokens 数；系数为每百万 tokens 美元价，故表达式值单位为
//     micro-USD；quota = round(expr × QuotaPerDollar / 1e6)（cache 读已计入
//     input，拆分避免重复计费）；表达式本身即终价，不叠乘组倍率（docs/04 公式实测节）；
//   - per_call：per_call_price × group_ratio × QuotaPerDollar。
func (e *Engine) Charge(price *ModelPrice, group string, u Usage) (int64, error) {
	if price == nil {
		return 0, errors.New("billing: 价格快照为空")
	}
	u.CacheRead = min(max(u.CacheRead, 0), u.Input)

	quotaPerUnit := float64(model.QuotaPerDollar)
	var q float64
	switch price.Mode {
	case ModeClassicRatio:
		gr := e.GroupRatio(group)
		q = (float64(u.Input) + float64(u.Completion)*price.CompletionRatio) *
			price.ModelRatio * gr * quotaPerUnit / 1e6
	case ModeTieredExpr:
		v, err := e.evalExpr(price.Expr, float64(u.Input-u.CacheRead), float64(u.Completion), float64(u.CacheRead))
		if err != nil {
			return 0, fmt.Errorf("计费表达式求值失败(模型 %s): %w", price.Model, err)
		}
		q = v * quotaPerUnit / 1e6
	case ModePerCall:
		gr := e.GroupRatio(group)
		q = price.PerCallPrice * gr * quotaPerUnit
	default:
		return 0, fmt.Errorf("billing: 未知计费模式 %q", price.Mode)
	}
	return roundQuota(q), nil
}

// Estimate 是预扣费估算：按输入粗估（请求体字节/4）与 max_tokens（缺省 1024）
// 计费后上浮 20%，四舍五入且最低 1 quota。响应完成后按实际 usage 多退少补（docs/04 第三节）。
func (e *Engine) Estimate(price *ModelPrice, group string, rawBody []byte, maxTokens int) int64 {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokensEstimate
	}
	prompt := len(rawBody) / BytesPerTokenEstimate
	actual, err := e.Charge(price, group, Usage{Input: prompt, Completion: maxTokens})
	if err != nil {
		// 估算失败理论不可达（Lookup 已验证表达式可编译）；防御性兜底为最小冻结额并记日志。
		log.Printf("[billing] 预扣估算失败(模型 %s): %v", price.Model, err)
		return 1
	}
	est := int64(math.Round(float64(actual) * EstimateHeadroom))
	if est < 1 {
		est = 1
	}
	return est
}

// roundQuota 四舍五入到整数 quota（负数钳制为 0：价格与用量非负，防御异常配置）。
func roundQuota(q float64) int64 {
	if q <= 0 {
		return 0
	}
	return int64(math.Round(q))
}

// ---- 运行轨配置解析缓存 ----

// settingsCache 是 options 计费键的一次解析结果，按 src 版本号失效重建。
type settingsCache struct {
	srcVersion      int64
	billingMode     map[string]string
	billingExpr     map[string]string
	billingPrice    map[string]float64
	modelRatio      map[string]float64
	completionRatio map[string]float64
	groupRatio      map[string]float64
}

// loadSettings 读取解析缓存；src 版本变化（配置热更）时整体重建。
// options 值 JSON 非法时忽略该键并记日志（回退到下一级价格来源，不中断服务）。
func (e *Engine) loadSettings() settingsCache {
	var version int64
	if v, ok := e.src.(interface{ Version() int64 }); ok {
		version = v.Version()
	}
	if s, ok := e.settings.Load().(settingsCache); ok && s.srcVersion == version {
		return s
	}

	s := settingsCache{
		srcVersion:      version,
		billingMode:     map[string]string{},
		billingExpr:     map[string]string{},
		billingPrice:    map[string]float64{},
		modelRatio:      map[string]float64{},
		completionRatio: map[string]float64{},
		groupRatio:      map[string]float64{},
	}
	if e.src != nil {
		decodeStringMap(e.src, OptionKeyBillingMode, s.billingMode)
		decodeStringMap(e.src, OptionKeyBillingExpr, s.billingExpr)
		decodeFloatMap(e.src, OptionKeyBillingPrice, s.billingPrice)
		decodeFloatMap(e.src, OptionKeyModelRatio, s.modelRatio)
		decodeFloatMap(e.src, OptionKeyCompletionRatio, s.completionRatio)
		decodeFloatMap(e.src, OptionKeyGroupRatio, s.groupRatio)
	}
	e.settings.Store(s)
	return s
}

// decodeStringMap 解析 options JSON 键到字符串映射；非法 JSON 记日志并留空。
func decodeStringMap(src Source, key string, out map[string]string) {
	raw, ok := src.Get(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		log.Printf("[billing] options 键 %s 非法 JSON，已忽略: %v", key, err)
	}
}

// decodeFloatMap 解析 options JSON 键到数值映射；非法 JSON 或负值项记日志并忽略（防错扣）。
func decodeFloatMap(src Source, key string, out map[string]float64) {
	raw, ok := src.Get(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		log.Printf("[billing] options 键 %s 非法 JSON，已忽略: %v", key, err)
		return
	}
	for k, v := range m {
		if v < 0 || v != v {
			log.Printf("[billing] options 键 %s 含非法负值项 %q，该项已忽略", key, k)
			continue
		}
		out[k] = v
	}
}
