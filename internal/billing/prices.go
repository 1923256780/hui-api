// 内置价格单（prices.json，go:embed 打包进二进制）：
//
//   - 作为 DB 配置之下的兜底价：options 运行轨（billing_setting.* / ModelRatio 等）
//     优先于本文件，二者都未命中才 ErrUnpriced；
//   - 启动时经 loadBuiltinPrices 做 schema 校验（version、mode 枚举、数值非负、
//     tiered_expr 表达式必须可编译），校验失败返回错误、进程启动失败——
//     价格单损坏时宁可拒绝全部计费请求；
//   - 真实模型价格属部署方配置（管理面写 options 热更），本文件只维护少量示例
//     与回归锚点。
package billing

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"

	"github.com/expr-lang/expr"
)

//go:embed prices.json
var pricesFS embed.FS

// BuiltinPricesVersion 是内置价格单的当前版本号（prices.json version 字段须一致，测试固化）。
const BuiltinPricesVersion = 1

// priceFile 是 prices.json 的线格式。
type priceFile struct {
	Version     int                    `json:"version"`
	UpdatedAt   string                 `json:"updated_at"`
	Description string                 `json:"description"`
	Models      map[string]priceEntry  `json:"models"`
}

// priceEntry 是单模型的内置价条目。
type priceEntry struct {
	Mode            Mode     `json:"mode"`
	Expr            string   `json:"expr,omitempty"`       // tiered_expr
	ModelRatio      *float64 `json:"model_ratio,omitempty"` // classic_ratio
	CompletionRatio *float64 `json:"completion_ratio,omitempty"`
	PerCallPrice    *float64 `json:"per_call_price,omitempty"` // per_call（美元/次）
}

// loadBuiltinPrices 读取、校验并展开内置价格单为模型名 → 价格快照。
func loadBuiltinPrices() (map[string]ModelPrice, error) {
	raw, err := pricesFS.ReadFile("prices.json")
	if err != nil {
		return nil, fmt.Errorf("读取内置价格单: %w", err)
	}
	var pf priceFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("解析内置价格单 JSON: %w", err)
	}
	if err := validatePriceFile(&pf); err != nil {
		return nil, err
	}
	out := make(map[string]ModelPrice, len(pf.Models))
	for name, entry := range pf.Models {
		p := ModelPrice{Model: name, Mode: entry.Mode}
		switch entry.Mode {
		case ModeTieredExpr:
			p.Expr = entry.Expr
		case ModeClassicRatio:
			p.ModelRatio = derefOrZero(entry.ModelRatio)
			p.CompletionRatio = derefOrZero(entry.CompletionRatio)
		case ModePerCall:
			p.PerCallPrice = derefOrZero(entry.PerCallPrice)
		}
		out[name] = p
	}
	return out, nil
}

// validatePriceFile 是 prices.json 的启动 schema 校验。
func validatePriceFile(pf *priceFile) error {
	if pf.Version < 1 {
		return fmt.Errorf("价格单 version 非法: %d（须 >= 1）", pf.Version)
	}
	if pf.Version != BuiltinPricesVersion {
		return fmt.Errorf("价格单 version %d 与代码锚点 %d 不一致，同步后随版本递增",
			pf.Version, BuiltinPricesVersion)
	}
	if len(pf.Models) == 0 {
		return fmt.Errorf("价格单 models 为空")
	}
	for name, entry := range pf.Models {
		switch entry.Mode {
		case ModeTieredExpr:
			if entry.Expr == "" {
				return fmt.Errorf("模型 %s: tiered_expr 缺少 expr", name)
			}
			program, err := compileExpr(entry.Expr)
			if err != nil {
				return fmt.Errorf("模型 %s: %w", name, err)
			}
			// 试求值（零用量）：把档位名/类型等运行期错误提前到启动校验。
			if _, err := expr.Run(program, evalEnv{}); err != nil {
				return fmt.Errorf("模型 %s: 表达式试求值失败: %w", name, err)
			}
		case ModeClassicRatio:
			if entry.ModelRatio == nil || *entry.ModelRatio < 0 || math.IsNaN(*entry.ModelRatio) {
				return fmt.Errorf("模型 %s: classic_ratio 缺少或非法 model_ratio", name)
			}
			if entry.CompletionRatio != nil && (*entry.CompletionRatio < 0 || math.IsNaN(*entry.CompletionRatio)) {
				return fmt.Errorf("模型 %s: 非法 completion_ratio", name)
			}
		case ModePerCall:
			if entry.PerCallPrice == nil || *entry.PerCallPrice < 0 || math.IsNaN(*entry.PerCallPrice) {
				return fmt.Errorf("模型 %s: per_call 缺少或非法 per_call_price", name)
			}
		default:
			return fmt.Errorf("模型 %s: 未知计费模式 %q", name, entry.Mode)
		}
	}
	return nil
}

func derefOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
