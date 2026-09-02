// 计费表达式求值（docs/04 tiered_expr 模式）：
//
//   - 表达式引擎为 expr-lang/expr（纯求值 DSL，白名单环境，无任意代码执行面）；
//   - 环境变量固定为 p / c / cr（原始 tokens 数：p=纯输入、c=输出、cr=缓存读；
//     单价由表达式中的系数表达，如 p * 0.15 表示 $0.15/百万 tokens；
//     乘积单位为 micro-USD，quota = round(值 × QuotaPerDollar / 1e6)）；
//   - 自定义函数 tier(name, value)：本波实现单层语义——仅接受 name="base" 并返回 value，
//     其余档位显式报错（将来扩展阶梯定价时不会静默错计费）；
//   - 编译结果经 programCache 按表达式文本并发安全缓存，求值（expr.Run）每次独立运行。
package billing

import (
	"fmt"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// TierBase 是 tier() 函数当前唯一支持的档位名（单层语义，见包注释）。
const TierBase = "base"

// evalEnv 是表达式求值环境：变量 p / c / cr（原始 tokens 数，见包注释口径）。
type evalEnv struct {
	P  float64 `expr:"p"`
	C  float64 `expr:"c"`
	CR float64 `expr:"cr"`
}

// compileExpr 编译计费表达式：浮点结果 + tier() 自定义函数 + 固定环境。
func compileExpr(src string) (*vm.Program, error) {
	program, err := expr.Compile(src,
		expr.Env(evalEnv{}),
		expr.AsFloat64(),
		expr.Function("tier", tierFunc, new(func(string, float64) float64)),
	)
	if err != nil {
		return nil, fmt.Errorf("编译计费表达式 %q: %w", src, err)
	}
	return program, nil
}

// tierFunc 是 tier(name, value) 的求值实现：单层语义（取第二参数表达式的值）。
// 非 "base" 档位显式报错——宁可拒绝请求，不可静默错扣。
func tierFunc(params ...any) (any, error) {
	if len(params) != 2 {
		return nil, fmt.Errorf("tier() 需要 2 个参数，收到 %d 个", len(params))
	}
	name, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("tier() 第一个参数须为字符串档位名")
	}
	if name != TierBase {
		return nil, fmt.Errorf("tier() 未支持的档位 %q（当前仅支持 %q）", name, TierBase)
	}
	v, ok := params[1].(float64)
	if !ok {
		return nil, fmt.Errorf("tier() 第二参数须为数值表达式")
	}
	return v, nil
}

// evalExpr 从缓存取编译结果并求值；未命中时编译并写入缓存（双检防重复）。
func (e *Engine) evalExpr(src string, p, c, cr float64) (float64, error) {
	program, err := e.programs().get(src)
	if err != nil {
		return 0, err
	}
	out, err := expr.Run(program, evalEnv{P: p, C: c, CR: cr})
	if err != nil {
		return 0, fmt.Errorf("求值计费表达式 %q: %w", src, err)
	}
	v, ok := out.(float64)
	if !ok {
		return 0, fmt.Errorf("计费表达式 %q 结果不是数值: %T", src, out)
	}
	return v, nil
}

// programs 取当前编译缓存（NewEngine 已初始化，Load 必然命中）。
func (e *Engine) programs() *programCache {
	return e.progCache.Load().(*programCache)
}

// programCache 是表达式编译缓存：表达式文本 → 编译产物，读写锁并发安全。
// 同一表达式的并发编译无害（结果一致），双检只避免重复写入。
type programCache struct {
	mu sync.RWMutex
	m  map[string]*vm.Program
}

func newProgramCache() *programCache {
	return &programCache{m: map[string]*vm.Program{}}
}

// get 返回缓存中的编译产物；未命中时编译（失败返回错误且不缓存）。
func (pc *programCache) get(src string) (*vm.Program, error) {
	pc.mu.RLock()
	program, ok := pc.m[src]
	pc.mu.RUnlock()
	if ok {
		return program, nil
	}
	program, err := compileExpr(src)
	if err != nil {
		return nil, err
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if p, ok := pc.m[src]; ok {
		return p, nil
	}
	pc.m[src] = program
	return program, nil
}
