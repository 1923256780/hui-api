package billing

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/1923256780/hui-api/internal/model"
)

// mapSource 是 Source 的内存 map 替身（测试注入 options 键值）。
type mapSource map[string]string

func (m mapSource) Get(k string) (string, bool) {
	v, ok := m[k]
	return v, ok
}

// newTestEngine 构造带 map 源的引擎；src 可为 nil。
func newTestEngine(t *testing.T, src Source) *Engine {
	t.Helper()
	e, err := NewEngine(src)
	if err != nil {
		t.Fatalf("构造计费引擎失败: %v", err)
	}
	return e
}

// ---- classic_ratio ----

// TestChargeClassicRatio 倍率模式基本公式：(p + c×cr_ratio) × mr × gr × 500000 / 1e6。
func TestChargeClassicRatio(t *testing.T) {
	e := newTestEngine(t, mapSource{
		OptionKeyModelRatio:      `{"m-a":0.15}`,
		OptionKeyCompletionRatio: `{"m-a":3.0}`,
		OptionKeyGroupRatio:      `{"vip":2.0}`,
	})
	price, err := e.LookupPrice("m-a")
	if err != nil {
		t.Fatalf("查询价格失败: %v", err)
	}
	if price.Mode != ModeClassicRatio || price.ModelRatio != 0.15 || price.CompletionRatio != 3.0 {
		t.Fatalf("价格快照不符: %+v", price)
	}
	// (100 + 200×3.0) × 0.15 × 1.0 × 500000 / 1e6 = 700 × 0.15 × 0.5 = 52.5 → 53
	q, err := e.Charge(price, DefaultGroup, Usage{Input: 100, Completion: 200})
	if err != nil {
		t.Fatalf("计费失败: %v", err)
	}
	if q != 53 {
		t.Fatalf("期望 53，实际 %d", q)
	}

	// 组倍率生效：vip=2.0 → 52.5×2 = 105
	q, _ = e.Charge(price, "vip", Usage{Input: 100, Completion: 200})
	if q != 105 {
		t.Fatalf("组倍率 vip 期望 105，实际 %d", q)
	}
}

// TestChargeClassicRatioRounding 四舍五入边界：0.5 向上、<0.5 向下、负值钳 0。
func TestChargeClassicRatioRounding(t *testing.T) {
	cases := []struct {
		mr    float64
		input int
		want  int64
	}{
		{0.15, 10, 1},  // 10×0.15×0.5 = 0.75 → 1
		{0.1, 99, 5},   // 99×0.1×0.5 = 4.95 → 5
		{0.1, 90, 5},   // 90×0.1×0.5 = 4.5 → 5（半数向上）
		{0.1, 89, 4},   // 89×0.1×0.5 = 4.45 → 4
		{0.0000001, 1, 0},
	}
	for i, c := range cases {
		e := newTestEngine(t, mapSource{OptionKeyModelRatio: fmt.Sprintf(`{"m":%v}`, c.mr)})
		price, _ := e.LookupPrice("m")
		q, err := e.Charge(price, DefaultGroup, Usage{Input: c.input})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if q != c.want {
			t.Fatalf("case %d: mr=%v input=%d 期望 %d，实际 %d", i, c.mr, c.input, c.want, q)
		}
	}
	if got := roundQuota(-3.6); got != 0 {
		t.Fatalf("负值应钳制为 0，实际 %d", got)
	}
}

// ---- tiered_expr ----

// TestChargeTieredExpr 表达式模式：p/c/cr 变量绑定与 cache 拆分防重复计费。
func TestChargeTieredExpr(t *testing.T) {
	e := newTestEngine(t, mapSource{
		OptionKeyBillingMode: `{"m-tier":"tiered_expr"}`,
		OptionKeyBillingExpr: `{"m-tier":"tier(\"base\", p * 0.15 + c * 0.5 + cr * 0.014)"}`,
	})
	price, err := e.LookupPrice("m-tier")
	if err != nil {
		t.Fatalf("查询价格失败: %v", err)
	}
	if price.Mode != ModeTieredExpr {
		t.Fatalf("模式应为 tiered_expr: %+v", price)
	}

	// 口径（docs/04 公式实测节）：变量 p/c/cr 为原始 tokens 数，系数为每百万
	// tokens 美元价，表达式值单位 micro-USD；quota = round(值 × 500000 / 1e6) = 值/2。
	// 无缓存：input=1000000, completion=1000000 → (1e6×0.15 + 1e6×0.5) m$ = 650000 → 325000
	q, err := e.Charge(price, DefaultGroup, Usage{Input: 1_000_000, Completion: 1_000_000})
	if err != nil {
		t.Fatalf("计费失败: %v", err)
	}
	if q != 325_000 {
		t.Fatalf("期望 325000，实际 %d", q)
	}

	// 缓存拆分：Input=1000（其中 CacheRead=400）→ p=600、cr=400、c=100
	// expr = 600×0.15 + 100×0.5 + 400×0.014 = 145.6 m$ → 145.6/2 = 72.8 → 73
	q, err = e.Charge(price, DefaultGroup, Usage{Input: 1000, Completion: 100, CacheRead: 400})
	if err != nil {
		t.Fatalf("计费失败: %v", err)
	}
	if q != 73 {
		t.Fatalf("缓存拆分期望 73，实际 %d", q)
	}

	// CacheRead 大于 Input 时钳制（防脏数据重复计费）。
	q, err = e.Charge(price, DefaultGroup, Usage{Input: 100, Completion: 0, CacheRead: 500})
	if err != nil {
		t.Fatalf("计费失败: %v", err)
	}
	// cache 钳到 100：p=0、cr=100 → 100×0.014 = 1.4 m$ → 0.7 → 1
	if q != 1 {
		t.Fatalf("cache 超限钳制期望 1，实际 %d", q)
	}
}

// TestTierFunction tier() 单层语义：base 返回第二参数值；其他档位显式报错。
func TestTierFunction(t *testing.T) {
	e := newTestEngine(t, nil)
	price := &ModelPrice{Model: "m", Mode: ModeTieredExpr, Expr: `tier("base", p * 2 + 1)`}
	q, err := e.Charge(price, DefaultGroup, Usage{Input: 1_000_000})
	if err != nil {
		t.Fatalf("tier(base) 求值失败: %v", err)
	}
	// p=1000000（原始 tokens）→ expr = 2000001 m$ → /2 = 1000000.5 → 1000001
	// （同时验证乘法、加法与四舍五入半数向上）。
	if want := int64(1_000_001); q != want {
		t.Fatalf("tier(base) 期望 %d，实际 %d", want, q)
	}

	priceGold := &ModelPrice{Model: "m", Mode: ModeTieredExpr, Expr: `tier("gold", p * 2)`}
	if _, err := e.Charge(priceGold, DefaultGroup, Usage{Input: 100}); err == nil {
		t.Fatal("tier(gold) 应显式报错（未实现的档位）")
	}
	priceBad := &ModelPrice{Model: "m", Mode: ModeTieredExpr, Expr: `tier(p, 1)`}
	if _, err := e.Charge(priceBad, DefaultGroup, Usage{Input: 100}); err == nil {
		t.Fatal("tier 第一参数非字符串应报错")
	}
}

// TestExprCacheConcurrent 并发同表达式：编译缓存无竞态、结果一致（-race 验证）。
func TestExprCacheConcurrent(t *testing.T) {
	e := newTestEngine(t, nil)
	price := &ModelPrice{Model: "m", Mode: ModeTieredExpr, Expr: `tier("base", p * 0.1 + c * 0.2)`}
	want, err := e.Charge(price, DefaultGroup, Usage{Input: 1_000_000, Completion: 500_000})
	if err != nil {
		t.Fatalf("预热计费失败: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := e.Charge(price, DefaultGroup, Usage{Input: 1_000_000, Completion: 500_000})
			if err != nil {
				errs <- err
				return
			}
			if q != want {
				errs <- fmt.Errorf("并发结果不一致: want %d got %d", want, q)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// ---- per_call ----

// TestChargePerCall 按次模式：固定价 × 组倍率 × 500000。
func TestChargePerCall(t *testing.T) {
	e := newTestEngine(t, mapSource{
		OptionKeyBillingMode:  `{"m-flat":"per_call"}`,
		OptionKeyBillingPrice: `{"m-flat":0.02}`,
		OptionKeyGroupRatio:   `{"vip":2.0}`,
	})
	price, err := e.LookupPrice("m-flat")
	if err != nil {
		t.Fatalf("查询价格失败: %v", err)
	}
	// 0.02 × 1.0 × 500000 = 10000
	q, err := e.Charge(price, DefaultGroup, Usage{Input: 999999, Completion: 999999})
	if err != nil {
		t.Fatalf("计费失败: %v", err)
	}
	if q != 10_000 {
		t.Fatalf("per_call 期望 10000，实际 %d", q)
	}
	// tokens 不影响按次计费；组倍率生效。
	q, _ = e.Charge(price, "vip", Usage{})
	if q != 20_000 {
		t.Fatalf("per_call vip 期望 20000，实际 %d", q)
	}
}

// ---- 未配价拒绝与配置优先级 ----

// TestLookupUnpricedRejected 无任何配置 → ErrUnpriced；声明不完整 → ErrUnpriced。
func TestLookupUnpricedRejected(t *testing.T) {
	e := newTestEngine(t, mapSource{})
	if _, err := e.LookupPrice("unknown-model"); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("未配价应返回 ErrUnpriced，实际 %v", err)
	}
	if _, err := e.LookupPrice(""); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("空模型名应返回 ErrUnpriced，实际 %v", err)
	}
	// 声明 tiered_expr 但缺表达式。
	e2 := newTestEngine(t, mapSource{OptionKeyBillingMode: `{"m":"tiered_expr"}`})
	if _, err := e2.LookupPrice("m"); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("缺表达式应返回 ErrUnpriced，实际 %v", err)
	}
	// 未知模式名。
	e3 := newTestEngine(t, mapSource{OptionKeyBillingMode: `{"m":"magic_mode"}`})
	if _, err := e3.LookupPrice("m"); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("未知模式应返回 ErrUnpriced，实际 %v", err)
	}
	// 错误信息不含内部细节（不含表达式/键名）。
	_, err := e3.LookupPrice("m")
	if err != nil && strings.Contains(err.Error(), "billing_setting") {
		t.Fatalf("错误信息不应暴露内部键名: %v", err)
	}
}

// TestBuiltinPriceFallback options 无配置时回退内置价单。
func TestBuiltinPriceFallback(t *testing.T) {
	e := newTestEngine(t, mapSource{})
	price, err := e.LookupPrice("example-tiered")
	if err != nil {
		t.Fatalf("内置价单模型应可查价: %v", err)
	}
	if price.Mode != ModeTieredExpr || price.Expr == "" {
		t.Fatalf("内置 tiered 模型价格不符: %+v", price)
	}
	if _, err := e.LookupPrice("example-classic"); err != nil {
		t.Fatalf("内置 classic 模型应可查价: %v", err)
	}
	if _, err := e.LookupPrice("example-per-call"); err != nil {
		t.Fatalf("内置 per_call 模型应可查价: %v", err)
	}
}

// TestDBPriorityOverBuiltin DB 配置优先于内置价单。
func TestDBPriorityOverBuiltin(t *testing.T) {
	e := newTestEngine(t, mapSource{
		OptionKeyBillingMode: `{"example-tiered":"per_call"}`,
		OptionKeyBillingPrice: `{"example-tiered":0.5}`,
	})
	price, err := e.LookupPrice("example-tiered")
	if err != nil {
		t.Fatalf("查价失败: %v", err)
	}
	if price.Mode != ModePerCall || price.PerCallPrice != 0.5 {
		t.Fatalf("DB 配置应覆盖内置价单: %+v", price)
	}
}

// TestEstimate 预扣估算：上浮 20%、最低 1。
func TestEstimate(t *testing.T) {
	e := newTestEngine(t, mapSource{
		OptionKeyModelRatio:      `{"m":1.0}`,
		OptionKeyCompletionRatio: `{"m":1.0}`,
	})
	price, _ := e.LookupPrice("m")
	// input=1000(4000B/4), completion=2000 → (1000+2000)×1×0.5 = 1500 → ×1.2 = 1800
	raw := make([]byte, 4000)
	if got := e.Estimate(price, DefaultGroup, raw, 2000); got != 1800 {
		t.Fatalf("估算期望 1800，实际 %d", got)
	}
	// max_tokens 缺省 1024：input=0 → 1024×0.5 = 512 → ×1.2 = 614.4 → 614
	if got := e.Estimate(price, DefaultGroup, nil, 0); got != 614 {
		t.Fatalf("缺省估算期望 614，实际 %d", got)
	}
	// 最小值 1。
	if got := e.Estimate(price, DefaultGroup, nil, 1); got < 1 {
		t.Fatalf("估算应 >= 1，实际 %d", got)
	}
}

// TestGroupRatioDefault 组倍率缺省 1.0、default 组可覆盖。
func TestGroupRatioDefault(t *testing.T) {
	e := newTestEngine(t, mapSource{})
	if got := e.GroupRatio(DefaultGroup); got != 1.0 {
		t.Fatalf("缺省组倍率应 1.0，实际 %v", got)
	}
	e2 := newTestEngine(t, mapSource{OptionKeyGroupRatio: `{"default":1.5,"vip":2.0}`})
	if got := e2.GroupRatio(DefaultGroup); got != 1.5 {
		t.Fatalf("default 组倍率应 1.5，实际 %v", got)
	}
	if got := e2.GroupRatio("vip"); got != 2.0 {
		t.Fatalf("vip 组倍率应 2.0，实际 %v", got)
	}
	if got := e2.GroupRatio("absent"); got != 1.5 {
		t.Fatalf("未配置组应回退 default 组倍率（1.5），实际 %v", got)
	}
}

// TestRoundTripQuotaUnit quota 单位锚点：500000 quota = $1（docs/03）。
func TestRoundTripQuotaUnit(t *testing.T) {
	if model.QuotaPerDollar != 500000 {
		t.Fatalf("QuotaPerDollar 锚点漂移: %d", model.QuotaPerDollar)
	}
}
