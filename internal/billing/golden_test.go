package billing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenCase 是黄金用例的线格式（中性字段名，不含任何实现细节耦合）。
type goldenCase struct {
	Name            string `json:"name"`
	Model           string `json:"model"`
	Mode            string `json:"mode"`
	Expr            string `json:"expr"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
	ExpectedQuota   int64  `json:"expected_quota"`
}

type goldenFile struct {
	Version     int          `json:"version"`
	Description string       `json:"description"`
	Cases       []goldenCase `json:"cases"`
}

// loadGoldenCases 读取黄金用例文件（internal/billing/testdata/golden/billing_cases.json）。
func loadGoldenCases(t *testing.T) goldenFile {
	t.Helper()
	path := filepath.Join("testdata", "golden", "billing_cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取黄金用例文件失败: %v", err)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("解析黄金用例文件失败: %v", err)
	}
	if gf.Version != 1 {
		t.Fatalf("黄金用例版本应 1，实际 %d", gf.Version)
	}
	if len(gf.Cases) != 4 {
		t.Fatalf("黄金用例应 4 条，实际 %d", len(gf.Cases))
	}
	return gf
}

// TestGoldenBilling 黄金测试集：四条实测账单样例按 tiered_expr 公式逐位断言
// （四舍五入口径一致）。任何计费相关改动必须全量跑通（AGENTS.md 第 5 节）。
func TestGoldenBilling(t *testing.T) {
	gf := loadGoldenCases(t)
	e := newTestEngine(t, nil)

	for _, tc := range gf.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			if tc.Mode != string(ModeTieredExpr) {
				t.Fatalf("黄金用例当前固化为 tiered_expr 口径，实际 %q", tc.Mode)
			}
			price := &ModelPrice{Model: tc.Model, Mode: ModeTieredExpr, Expr: tc.Expr}
			got, err := e.Charge(price, DefaultGroup, Usage{
				Input:      tc.InputTokens,
				Completion: tc.OutputTokens,
				CacheRead:  tc.CacheReadTokens,
			})
			if err != nil {
				t.Fatalf("计费求值失败: %v", err)
			}
			if got != tc.ExpectedQuota {
				t.Fatalf("黄金断言失败：期望 quota=%d，实际 %d（tokens: in=%d out=%d cache=%d）",
					tc.ExpectedQuota, got, tc.InputTokens, tc.OutputTokens, tc.CacheReadTokens)
			}
		})
	}
}

// TestGoldenCasesViaLookup 黄金用例经完整配置链路（options 注入 → LookupPrice → Charge）：
// 保证配置解析路径与直构快照路径口径一致。
func TestGoldenCasesViaLookup(t *testing.T) {
	gf := loadGoldenCases(t)

	exprs := map[string]string{}
	for _, tc := range gf.Cases {
		exprs[tc.Model] = tc.Expr
	}
	modeJSON, _ := json.Marshal(func() map[string]string {
		m := map[string]string{}
		for _, tc := range gf.Cases {
			m[tc.Model] = string(ModeTieredExpr)
		}
		return m
	}())
	exprJSON, _ := json.Marshal(exprs)

	e := newTestEngine(t, mapSource{
		OptionKeyBillingMode: string(modeJSON),
		OptionKeyBillingExpr: string(exprJSON),
	})

	for _, tc := range gf.Cases {
		price, err := e.LookupPrice(tc.Model)
		if err != nil {
			t.Fatalf("%s: 查价失败: %v", tc.Name, err)
		}
		got, err := e.Charge(price, DefaultGroup, Usage{
			Input:      tc.InputTokens,
			Completion: tc.OutputTokens,
			CacheRead:  tc.CacheReadTokens,
		})
		if err != nil {
			t.Fatalf("%s: 计费失败: %v", tc.Name, err)
		}
		if got != tc.ExpectedQuota {
			t.Fatalf("%s: 经配置链路黄金断言失败：期望 %d，实际 %d", tc.Name, tc.ExpectedQuota, got)
		}
	}
}

// TestGoldenFileNoCompetitorWords 黄金用例文件竞品词自检（与 CI 扫描语义一致：
// 大小写不敏感、子串匹配）。词表以 .github/competitor-words.txt 为单一事实源，
// 测试代码内不硬编码禁用词（否则本文件自身会成为扫描命中源）。
func TestGoldenFileNoCompetitorWords(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", "billing_cases.json"))
	if err != nil {
		t.Fatalf("读取黄金用例文件失败: %v", err)
	}
	// go test 的 CWD 为包目录（internal/billing），仓库根即 ../..。
	wordsRaw, err := os.ReadFile(filepath.Join("..", "..", ".github", "competitor-words.txt"))
	if err != nil {
		t.Fatalf("读取竞品词表失败（自检必须可执行）: %v", err)
	}
	words := []string{}
	for _, line := range strings.Split(string(wordsRaw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words = append(words, strings.ToLower(line))
	}
	if len(words) == 0 {
		t.Fatal("竞品词表为空，自检无法执行")
	}
	content := strings.ToLower(string(raw))
	for _, w := range words {
		if strings.Contains(content, w) {
			t.Fatalf("黄金用例文件包含禁用词 %q", w)
		}
	}
}

// TestBuiltinPricesValid 内置价格单启动校验：NewEngine 全链路成功即 schema 通过。
func TestBuiltinPricesValid(t *testing.T) {
	if _, err := NewEngine(nil); err != nil {
		t.Fatalf("内置价格单校验失败: %v", err)
	}
}

// TestValidatePriceFileRejectsBad 校验器拒绝非法价单（版本/模式/表达式/负值）。
func TestValidatePriceFileRejectsBad(t *testing.T) {
	f := func(mr float64) *float64 { return &mr }
	bad := []priceFile{
		{Version: 0, Models: map[string]priceEntry{"m": {Mode: ModePerCall, PerCallPrice: f(0.1)}}},
		{Version: 2, Models: map[string]priceEntry{"m": {Mode: ModePerCall, PerCallPrice: f(0.1)}}},
		{Version: 1, Models: map[string]priceEntry{}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: "magic"}}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: ModeTieredExpr}}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: ModeTieredExpr, Expr: `tier("gold", p)`}}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: ModeClassicRatio}}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: ModeClassicRatio, ModelRatio: f(-0.1)}}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: ModePerCall}}},
		{Version: 1, Models: map[string]priceEntry{"m": {Mode: ModePerCall, PerCallPrice: f(-1)}}},
	}
	for i := range bad {
		if err := validatePriceFile(&bad[i]); err == nil {
			t.Fatalf("case %d 应校验失败", i)
		}
	}
	// 合法样例应通过。
	good := priceFile{Version: 1, Models: map[string]priceEntry{
		"a": {Mode: ModeClassicRatio, ModelRatio: f(0.1)},
		"b": {Mode: ModeTieredExpr, Expr: `tier("base", p * 0.15)`},
		"c": {Mode: ModePerCall, PerCallPrice: f(0.02)},
	}}
	if err := validatePriceFile(&good); err != nil {
		t.Fatalf("合法价单不应报错: %v", err)
	}
}

// TestGoldenErrorsAreWrapped 未配价错误用 %w 包装，errors.Is 判定可用。
func TestGoldenErrorsAreWrapped(t *testing.T) {
	e := newTestEngine(t, mapSource{})
	_, err := e.LookupPrice("ghost-model")
	if !errors.Is(err, ErrUnpriced) {
		t.Fatalf("errors.Is(ErrUnpriced) 应命中，实际 %v", err)
	}
}
