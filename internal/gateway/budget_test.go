// budget_test.go 令牌预算周期惰性重置测试（M2-wave3）：
// 覆盖首次边界初始化、过期窗口滚动复原 remain、跨多周期相位对齐、monthly 日
// 钳制、并发滚动恰一次重置（-race）、unlimited/无周期/非法取值零成本放行。
package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// mustTokenID 按明文 key 反查令牌 ID。
func mustTokenID(t *testing.T, st *store.Store, plain string) int64 {
	t.Helper()
	var tok model.Token
	if err := st.Read.Where("key_hash = ?", HashKey(plain)).First(&tok).Error; err != nil {
		t.Fatalf("查询令牌失败: %v", err)
	}
	return tok.ID
}

// chatUsageUpstream 返回固定 usage 的假上游构造器（p/c tokens 由参数给定）。
func chatUsageUpstream(p, comp int) func(t *testing.T) (string, *[]upstreamHit) {
	return func(t *testing.T) (string, *[]upstreamHit) {
		body := fmt.Sprintf(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, p, comp)
		up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		return up.URL, hits
	}
}

// seedBudgetToken 写入一枚非 unlimited、带预算周期的令牌并返回明文。
func seedBudgetToken(t *testing.T, st *store.Store, mutate func(*model.Token)) string {
	t.Helper()
	return seedToken(t, st, func(tok *model.Token) {
		tok.UnlimitedQuota = false
		tok.Quota = 100000
		tok.RemainQuota = 100000
		if mutate != nil {
			mutate(tok)
		}
	})
}

// tokenRow 读取令牌行。
func tokenRow(t *testing.T, st *store.Store, id int64) model.Token {
	t.Helper()
	var tok model.Token
	if err := st.Read.First(&tok, id).Error; err != nil {
		t.Fatalf("读取令牌失败: %v", err)
	}
	return tok
}

// budgetLogs 返回 protocol=budget 的日志行。
func budgetLogs(t *testing.T, st *store.Store) []model.Log {
	t.Helper()
	var rows []model.Log
	if err := st.Read.Where("protocol = ?", "budget").Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("查询 budget 日志失败: %v", err)
	}
	return rows
}

// seedM1Channel 写入一个 m1 模型渠道（返回上游地址占位）。
func seedM1Channel(t *testing.T, st *store.Store, url string) {
	t.Helper()
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: url, Key: "k", Models: "m1"})
}

// TestBudgetInitOnFirstRequest 首次请求初始化边界：reset_at 0 → now+窗口，remain 只减不重置。
func TestBudgetInitOnFirstRequest(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)
	plain := seedBudgetToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "7d"
		tok.BudgetResetAt = 0
	})
	tokID := mustTokenID(t, st, plain)

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	row := tokenRow(t, st, tokID)
	if row.BudgetResetAt <= time.Now().Unix() {
		t.Fatalf("reset_at 应落在未来，实际 %d", row.BudgetResetAt)
	}
	if diff := row.BudgetResetAt - time.Now().Unix(); diff > 7*24*3600 {
		t.Fatalf("7d 窗口的 reset_at 应在 7d 内，偏移 %d 秒", diff)
	}
	if n := len(budgetLogs(t, st)); n != 0 {
		t.Fatalf("首次初始化不是重置，不应写 budget 日志，实际 %d 条", n)
	}
}

// TestBudgetRollOnExpired 过期窗口滚动：remain 复原为 quota、reset_at 相位前移、恰一条重置日志。
func TestBudgetRollOnExpired(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)

	old := time.Now().Unix() - 100
	plain = seedBudgetToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "7d"
		tok.BudgetResetAt = old
		tok.RemainQuota = 37 // 模拟旧窗口已消耗
	})
	tokID := mustTokenID(t, st, plain)

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	row := tokenRow(t, st, tokID)
	// 7d 窗口：old+7d 必然覆盖 now（100s < 7d），cursor 精确等于 old+7d。
	if row.BudgetResetAt != old+7*24*3600 {
		t.Fatalf("reset_at 应为 old+7d=%d，实际 %d", old+7*24*3600, row.BudgetResetAt)
	}
	// remain 复原为 quota(100000) 后再扣本次实结（p=12,c=8 的 tiered 计费远小于 1000）。
	if row.RemainQuota <= 99000 || row.RemainQuota > 100000 {
		t.Fatalf("remain 应复原后扣本次实结（99000,100000]，实际 %d", row.RemainQuota)
	}

	logs := budgetLogs(t, st)
	if len(logs) != 1 {
		t.Fatalf("应恰好 1 条重置日志，实际 %d 条", len(logs))
	}
	lg := logs[0]
	if lg.TokenID != tokID || lg.ModelName != "budget_reset" {
		t.Fatalf("重置日志归属不符: %+v", lg)
	}
	if lg.Detail == "" || !strings.Contains(lg.Detail, `"event":"budget_reset"`) {
		t.Fatalf("重置日志 detail 应含 event=budget_reset: %q", lg.Detail)
	}
}

// TestBudgetPhaseAlignMultiPeriod 跨多周期过期：逐窗口步进保持相位（old+2*窗口）。
func TestBudgetPhaseAlignMultiPeriod(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)

	old := time.Now().Unix() - 30*3600 // 跨 2 个 24h 窗口（+24h 仍≤now，+48h 覆盖 now）
	plain := seedBudgetToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "24h"
		tok.BudgetResetAt = old
		tok.RemainQuota = 5
	})
	tokID := mustTokenID(t, st, plain)

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	row := tokenRow(t, st, tokID)
	if row.BudgetResetAt != old+48*3600 {
		t.Fatalf("reset_at 应步进到 old+48h=%d，实际 %d", old+48*3600, row.BudgetResetAt)
	}
	if row.RemainQuota <= 99000 {
		t.Fatalf("remain 应已复原，实际 %d", row.RemainQuota)
	}
}

// TestMonthlyClamp 月边界钳制：同号日缺失钳到月末（含闰年），跨年自动进位。
func TestMonthlyClamp(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		from time.Time
		want time.Time
	}{
		{time.Date(2026, 1, 31, 8, 0, 0, 0, loc), time.Date(2026, 2, 28, 8, 0, 0, 0, loc)},
		{time.Date(2024, 1, 29, 8, 0, 0, 0, loc), time.Date(2024, 2, 29, 8, 0, 0, 0, loc)}, // 闰年
		{time.Date(2026, 12, 31, 8, 0, 0, 0, loc), time.Date(2027, 1, 31, 8, 0, 0, 0, loc)}, // 跨年
		{time.Date(2026, 3, 15, 8, 0, 0, 0, loc), time.Date(2026, 4, 15, 8, 0, 0, 0, loc)},
	}
	for i, c := range cases {
		if got := addMonthsClamped(c.from, 1); !got.Equal(c.want) {
			t.Fatalf("case %d: 应为 %s，实际 %s", i, c.want, got)
		}
	}
}

// TestBudgetRollMonthly 令牌级 monthly 滚动：边界按自然月推进且相位保持。
func TestBudgetRollMonthly(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)

	old := time.Now().AddDate(0, -2, 0) // 两个月前：跨 2 个自然月
	plain := seedBudgetToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "monthly"
		tok.BudgetResetAt = old.Unix()
		tok.RemainQuota = 9
	})
	tokID := mustTokenID(t, st, plain)

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	row := tokenRow(t, st, tokID)
	// 从 old 起逐月步进到覆盖 now 的边界：预期恰好 old+2 个月（±1s 由时区无关的
	// unix 比对承担；同号日跨月钳制只在 29-31 号触发，断言用 AddDate 对照）。
	want := old
	for !want.After(time.Now()) {
		want = addMonthsClamped(want, 1)
	}
	if row.BudgetResetAt != want.Unix() {
		t.Fatalf("monthly 边界应步进到 %d，实际 %d", want.Unix(), row.BudgetResetAt)
	}
	if len(budgetLogs(t, st)) != 1 {
		t.Fatalf("应恰好 1 条重置日志")
	}
}

// TestBudgetConcurrentSingleReset 并发滚动：N 请求同抢过期令牌，恰一条重置日志、
// 边界只前移一次（CAS 语义，-race 下验证）。
func TestBudgetConcurrentSingleReset(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)

	old := time.Now().Unix() - 100
	plain := seedBudgetToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "7d"
		tok.BudgetResetAt = old
		tok.RemainQuota = 37
	})
	tokID := mustTokenID(t, st, plain)
	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			w := postChat(t, g, plain, body)
			codes[idx] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("并发请求 %d 应 200，实际 %d", i, code)
		}
	}

	row := tokenRow(t, st, tokID)
	if row.BudgetResetAt != old+7*24*3600 {
		t.Fatalf("reset_at 应为 old+7d，实际 %d", row.BudgetResetAt)
	}
	if n := len(budgetLogs(t, st)); n != 1 {
		t.Fatalf("并发下应恰好 1 条重置日志，实际 %d 条", n)
	}
	if row.RemainQuota <= 98000 {
		t.Fatalf("并发结算后 remain 应接近满额，实际 %d", row.RemainQuota)
	}
}

// TestBudgetSkipCases 零成本放行：unlimited 令牌、无周期令牌、非法取值均不触库。
func TestBudgetSkipCases(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := chatUsageUpstream(12, 8)(t)
	seedM1Channel(t, st, up)

	expired := time.Now().Unix() - 50
	unlim := seedToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "7d"
		tok.BudgetResetAt = expired
	})
	noPeriod := seedToken(t, st, func(tok *model.Token) {
		tok.UnlimitedQuota = false
		tok.Quota = 1000
		tok.RemainQuota = 10
	})
	unknownVal := seedBudgetToken(t, st, func(tok *model.Token) {
		tok.BudgetDuration = "fortnight"
		tok.BudgetResetAt = expired
	})

	for _, plain := range []string{unlim, noPeriod, unknownVal} {
		w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))
		if w.Code != http.StatusOK {
			t.Fatalf("%s 应 200，实际 %d body=%s", plain, w.Code, w.Body.String())
		}
	}

	// 三枚令牌的边界均保持原值：unlimited 不参与预算、无周期无边界、非法取值降级。
	if row := tokenRow(t, st, mustTokenID(t, st, unlim)); row.BudgetResetAt != expired {
		t.Fatalf("unlimited 令牌不应滚动，reset_at %d", row.BudgetResetAt)
	}
	if row := tokenRow(t, st, mustTokenID(t, st, noPeriod)); row.BudgetResetAt != 0 {
		t.Fatalf("无周期令牌不应落边界，reset_at %d", row.BudgetResetAt)
	}
	if row := tokenRow(t, st, mustTokenID(t, st, unknownVal)); row.BudgetResetAt != expired {
		t.Fatalf("非法取值令牌不应滚动，reset_at %d", row.BudgetResetAt)
	}
	if n := len(budgetLogs(t, st)); n != 0 {
		t.Fatalf("不应产生 budget 日志，实际 %d 条", n)
	}
}
