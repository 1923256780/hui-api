package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// ---- 计费挂接端到端测试（Serve 编排 × billing 内核，docs/04 第三、四节）----
//
// newTestGateway 默认注入 ModelRatio {"m1","claude-x","absent"}（隐式 classic 价），
// 保证编排级既有测试不被未配价拒绝；本文件测试按需注入显式计费配置，
// 并用 seedToken 的 mutate 钩子造非 unlimited 带余额令牌走完整冻结/结算链路。

// seedQuotaToken 写入一枚非 unlimited、带余额的令牌并返回明文。
func seedQuotaToken(t *testing.T, st *store.Store, remain int64) string {
	t.Helper()
	return seedToken(t, st, func(tk *model.Token) {
		tk.UnlimitedQuota = false
		tk.RemainQuota = remain
	})
}

// reloadToken 按明文 key 重新读取令牌（断言余额变化）。
func reloadToken(t *testing.T, st *store.Store, plain string) model.Token {
	t.Helper()
	var tok model.Token
	if err := st.Read.Where("key_hash = ?", HashKey(plain)).First(&tok).Error; err != nil {
		t.Fatalf("读取令牌失败: %v", err)
	}
	return tok
}

// logRowsAfterClose 排空异步日志后读取全部日志行（确定性落库断言）。
func logRowsAfterClose(t *testing.T, g *Gateway, st *store.Store) []model.Log {
	t.Helper()
	g.Close() // 排空：与 main 停机同路径
	var rows []model.Log
	if err := st.Read.Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	return rows
}

// decodeLogDetail 反序列化日志 detail 列（计费依据 JSON）。
func decodeLogDetail(t *testing.T, row model.Log) billing.Detail {
	t.Helper()
	var d billing.Detail
	if err := json.Unmarshal([]byte(row.Detail), &d); err != nil {
		t.Fatalf("detail 应为合法 JSON: %v (%s)", err, row.Detail)
	}
	return d
}

// tieredOptions 返回 m1 的 tiered_expr 显式配置（p×0.1 + c×0.2，单位 micro-USD）。
func tieredOptions() map[string]string {
	return map[string]string{
		billing.OptionKeyBillingMode: `{"m1":"tiered_expr"}`,
		billing.OptionKeyBillingExpr: `{"m1":"tier(\"base\", p * 0.1 + c * 0.2)"}`,
	}
}

// TestGatewaySettlement 正常结算：按实际 usage 计费、冻结退差、余额同步扣减、
// 日志落库且 detail 记录计费依据（可反向重算）。
func TestGatewaySettlement(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 100000)
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`)
	w := postChat(t, g, plain, body)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 实结：(100×0.1 + 50×0.2) micro-USD = 20 → quota = 20×500000/1e6 = 10。
	const wantActual = int64(10)
	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != 100000-wantActual {
		t.Fatalf("结算后余额应为 %d（多退少补后实扣 10），实际 %d", 100000-wantActual, tok.RemainQuota)
	}

	// 冻结额同口径复算：应大于实结（证明退差确实发生）。
	price, err := g.price.LookupPrice("m1")
	if err != nil {
		t.Fatalf("查价失败: %v", err)
	}
	frozen := g.price.Estimate(price, billing.DefaultGroup, body, 0)
	if frozen <= wantActual {
		t.Fatalf("冻结额 %d 应大于实结 %d（否则退差不可观测）", frozen, wantActual)
	}

	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 {
		t.Fatalf("应落库 1 条请求日志，实际 %d", len(rows))
	}
	row := rows[0]
	if row.ModelName != "m1" || row.PromptTokens != 100 || row.CompletionTokens != 50 || row.Quota != wantActual {
		t.Fatalf("日志字段不符: %+v", row)
	}
	d := decodeLogDetail(t, row)
	if d.Mode != string(billing.ModeTieredExpr) || d.Frozen != frozen || d.Estimated || d.Aborted {
		t.Fatalf("detail 不符: %+v（frozen 期望 %d）", d, frozen)
	}
}

// TestGatewaySettleOvercharge 实结超过冻结（上游无视 max_tokens）：补扣允许透支到负数。
func TestGatewaySettleOvercharge(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 1000)
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"big"}}],"usage":{"prompt_tokens":100,"completion_tokens":5000000}}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	// max_tokens=1 压低冻结额；上游实际返回 5M completion tokens。
	body := []byte(`{"model":"m1","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	w := postChat(t, g, plain, body)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 实结：(100×0.1 + 5000000×0.2) micro-USD = 1000010 → quota = 500005；
	// 补扣后余额 = 1000 - 500005 = -499005（负数，冻结路径将拒绝后续请求）。
	const wantRemain = int64(1000) - 500005
	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != wantRemain {
		t.Fatalf("补扣应允许透支到 %d，实际 %d", wantRemain, tok.RemainQuota)
	}
}

// TestGatewayStreamAbortRefund 流中断：Respond 报错 → 全额退还冻结并标记 aborted。
func TestGatewayStreamAbortRefund(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 100000)
	// 声明大 Content-Length 但只写一个事件即断连 → 客户端 unexpected EOF（流中断）。
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Content-Length", "4096")
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	body := []byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	w := postChat(t, g, plain, body)
	if w.Code != http.StatusOK {
		t.Fatalf("流式响应头已写出应 200，实际 %d", w.Code)
	}

	// 全额退款：余额恢复到冻结前。
	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != 100000 {
		t.Fatalf("流中断应全额退款，余额应恢复 100000，实际 %d", tok.RemainQuota)
	}

	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 {
		t.Fatalf("应落库 1 条 aborted 日志，实际 %d", len(rows))
	}
	d := decodeLogDetail(t, rows[0])
	if !d.Aborted || !d.RefundFull {
		t.Fatalf("流中断日志应标记 aborted+refund_full：%+v", d)
	}
	if rows[0].Quota != 0 {
		t.Fatalf("流中断实结应为 0，实际 %d", rows[0].Quota)
	}
}

// TestGatewayEstimatedBilling usage 缺失但有正常内容：本地粗估并标记 estimated。
func TestGatewayEstimatedBilling(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 100000)
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`)
	w := postChat(t, g, plain, body)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 粗估口径复算：输入 = 请求体字节/4，输出 = 响应字节/4。
	price, err := g.price.LookupPrice("m1")
	if err != nil {
		t.Fatalf("查价失败: %v", err)
	}
	actual, err := g.price.Charge(price, billing.DefaultGroup, billing.Usage{
		Input:      len(body) / billing.BytesPerTokenEstimate,
		Completion: w.Body.Len() / billing.BytesPerTokenEstimate,
	})
	if err != nil {
		t.Fatalf("粗估计费失败: %v", err)
	}
	if actual <= 0 {
		t.Fatalf("粗估 quota 应为正，实际 %d", actual)
	}

	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != 100000-actual {
		t.Fatalf("粗估结算后余额应为 %d，实际 %d", 100000-actual, tok.RemainQuota)
	}

	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 {
		t.Fatalf("应落库 1 条日志，实际 %d", len(rows))
	}
	if rows[0].Quota != actual || rows[0].PromptTokens != len(body)/billing.BytesPerTokenEstimate {
		t.Fatalf("粗估日志不符: %+v（期望 quota=%d prompt=%d）",
			rows[0], actual, len(body)/billing.BytesPerTokenEstimate)
	}
	d := decodeLogDetail(t, rows[0])
	if !d.Estimated || d.Aborted {
		t.Fatalf("应标记 estimated 且非 aborted: %+v", d)
	}
}

// TestGatewayUnpricedModel503 未配价模型显式拒绝：发生在渠道选择之前，无日志无账本副作用。
func TestGatewayUnpricedModel503(t *testing.T) {
	g, st, plain := newTestGateway(t, nil) // 默认价不含 ghost-model
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		t.Fatal("未配价拒绝不应触达上游")
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "*"})
	_ = hits

	w := postChat(t, g, plain, []byte(`{"model":"ghost-model","messages":[]}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("应 503，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_not_priced") {
		t.Fatalf("错误码应为 model_not_priced: %s", w.Body.String())
	}

	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 0 {
		t.Fatalf("未配价拒绝不应产生日志，实际 %d 条", len(rows))
	}
}

// TestGatewayInsufficientQuota403 余额不足冻结失败：403 语义错误且无副作用。
func TestGatewayInsufficientQuota403(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 5)
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		t.Fatal("冻结失败不应触达上游")
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})
	_ = hits

	body := []byte(`{"model":"m1","max_tokens":8192,"messages":[{"role":"user","content":"hello"}]}`)
	w := postChat(t, g, plain, body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("应 403，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_quota") {
		t.Fatalf("错误码应为 insufficient_quota: %s", w.Body.String())
	}

	// 余额原封不动。
	if tok := reloadToken(t, st, plain); tok.RemainQuota != 5 {
		t.Fatalf("冻结失败余额应保持 5，实际 %d", tok.RemainQuota)
	}
	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 0 {
		t.Fatalf("冻结失败不应产生日志，实际 %d 条", len(rows))
	}
}

// TestGatewayUnlimitedSkipsLedger unlimited 令牌：正常计费落日志，账本零副作用。
func TestGatewayUnlimitedSkipsLedger(t *testing.T) {
	g, st, plain := newTestGateway(t, tieredOptions())
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	tok := reloadToken(t, st, plain)
	if !tok.UnlimitedQuota || tok.RemainQuota != 0 {
		t.Fatalf("unlimited 令牌账本不应变动: %+v", tok)
	}

	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 {
		t.Fatalf("应落库 1 条日志，实际 %d", len(rows))
	}
	if rows[0].Quota != 10 {
		t.Fatalf("unlimited 仍应按 usage 计费落日志（quota=10），实际 %d", rows[0].Quota)
	}
	d := decodeLogDetail(t, rows[0])
	if !d.Unlimited || d.Frozen != 0 {
		t.Fatalf("detail 应标记 unlimited 且 frozen=0: %+v", d)
	}
}

// TestGatewayClassicRatioOverWire classic_ratio 经 Serve 全链路（ModelRatio/CompletionRatio 回退价）。
func TestGatewayClassicRatioOverWire(t *testing.T) {
	g, st, _ := newTestGateway(t, map[string]string{
		billing.OptionKeyModelRatio:      `{"m1":0.5,"claude-x":0.5,"absent":0.5}`,
		billing.OptionKeyCompletionRatio: `{"m1":3.0}`,
	})
	plain := seedQuotaToken(t, st, 100000)
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// (100 + 50×3) × 0.5 × 500000/1e6 = 62.5 → 63。
	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != 100000-63 {
		t.Fatalf("classic_ratio 实结应 63，余额应 %d，实际 %d", 100000-63, tok.RemainQuota)
	}
	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 || rows[0].Quota != 63 {
		t.Fatalf("classic_ratio 日志 quota 应 63: %+v", rows)
	}
	d := decodeLogDetail(t, rows[0])
	if d.Mode != string(billing.ModeClassicRatio) || d.ModelRatio != 0.5 || d.CompRatio != 3.0 {
		t.Fatalf("classic_ratio detail 不符: %+v", d)
	}
}

// TestGatewayPerCallOverWire per_call 经 Serve 全链路：固定价 × group_ratio × 500000。
func TestGatewayPerCallOverWire(t *testing.T) {
	g, st, _ := newTestGateway(t, map[string]string{
		billing.OptionKeyBillingMode:  `{"m1":"per_call"}`,
		billing.OptionKeyBillingPrice: `{"m1":0.02}`,
	})
	plain := seedQuotaToken(t, st, 100000)
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 0.02 × 1.0 × 500000 = 10000（按次计费与 usage 无关）。
	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != 100000-10000 {
		t.Fatalf("per_call 实结应 10000，余额应 %d，实际 %d", 100000-10000, tok.RemainQuota)
	}
	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 || rows[0].Quota != 10000 {
		t.Fatalf("per_call 日志 quota 应 10000: %+v", rows)
	}
	d := decodeLogDetail(t, rows[0])
	if d.Mode != string(billing.ModePerCall) {
		t.Fatalf("per_call detail 不符: %+v", d)
	}
}

// TestGatewayLogsDrainOnClose 并发请求的日志经 Close 排空后全量落库（停机 drain 语义）。
func TestGatewayLogsDrainOnClose(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 10000000)
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	const n = 20
	for i := 0; i < n; i++ {
		w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
		if w.Code != http.StatusOK {
			t.Fatalf("请求 %d 应 200，实际 %d body=%s", i, w.Code, w.Body.String())
		}
	}

	// 不等待定时窗口，直接停机排空：n 条日志必须全部落库。
	rows := logRowsAfterClose(t, g, st)
	if int64(len(rows)) != n {
		t.Fatalf("停机排空后应落库 %d 条，实际 %d", n, len(rows))
	}
	for _, row := range rows {
		if row.Quota != 1 { // (10×0.1 + 5×0.2) = 2 micro-USD → 1 quota
			t.Fatalf("每请求实结应 1 quota: %+v", row)
		}
	}
}

// TestGatewayAbortedLogBeforeSettle defer 兜底退款路径（未走显式结算即退出）：
// 单渠道上游 500 → 换点穷尽（排除后无渠道）→ 503 退出。此时冻结已发生但从未
// 结算，兜底逻辑必须全额退款并恰好产生一条 aborted 日志（Serve 契约）。
func TestGatewayAbortedLogBeforeSettle(t *testing.T) {
	g, st, _ := newTestGateway(t, tieredOptions())
	plain := seedQuotaToken(t, st, 100000)
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	w := postChat(t, g, plain, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("换点穷尽应 503，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(*hits) != 1 {
		t.Fatalf("单渠道首发失败后即无候选，应只打 1 次上游，实际 %d", len(*hits))
	}

	// 冻结额应全额退还（请求未成功）。
	tok := reloadToken(t, st, plain)
	if tok.RemainQuota != 100000 {
		t.Fatalf("请求未成功应全额退款，余额应恢复 100000，实际 %d", tok.RemainQuota)
	}

	rows := logRowsAfterClose(t, g, st)
	if len(rows) != 1 {
		t.Fatalf("未成功请求应恰好落库 1 条 aborted 日志，实际 %d", len(rows))
	}
	d := decodeLogDetail(t, rows[0])
	if !d.Aborted || !d.RefundFull {
		t.Fatalf("兜底日志应标记 aborted+refund_full: %+v", d)
	}
}
