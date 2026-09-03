package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
)

// testClientIP 是 httptest.NewRequest 的默认客户端 IP（RemoteAddr=192.0.2.1:n）。
const testClientIP = "192.0.2.1"

// okUpstream 返回标准成功响应（带 usage，供 TPM 记账与计费断言）。
func okUpstream() func(w http.ResponseWriter, r *http.Request, body []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`))
	}
}

// TestGatewayRequestRateLimit429 全局请求限流：窗口内请求数达上限后 429 +
// Retry-After；本机限流不触上游（hits 不增）、不计入熔断（CooldownUntil 零值）。
func TestGatewayRequestRateLimit429(t *testing.T) {
	g, st, plain := newTestGateway(t, map[string]string{
		OptionKeyRateLimitEnabled:         "true",
		OptionKeyRateLimitDurationMinutes: "1",
		OptionKeyRateLimitCount:           "2",
	})
	up, hits := fakeUpstream(t, okUpstream())
	ch := seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	w1 := postChat(t, g, plain, body)
	w2 := postChat(t, g, plain, body)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("窗口内前 2 次应放行，实际 %d / %d", w1.Code, w2.Code)
	}

	w3 := postChat(t, g, plain, body)
	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次应 429，实际 %d body=%s", w3.Code, w3.Body.String())
	}
	if !strings.Contains(w3.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("错误码应为 rate_limit_exceeded: %s", w3.Body.String())
	}
	ra, err := strconv.Atoi(w3.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Fatalf("Retry-After 应为 (0,60] 内整数，实际 %q err=%v", w3.Header().Get("Retry-After"), err)
	}
	if len(*hits) != 2 {
		t.Fatalf("限流发生在渠道选择之前：上游应只收到 2 次请求，实际 %d", len(*hits))
	}
	if cd := g.Breaker().CooldownUntil(ch.ID); !cd.IsZero() {
		t.Fatalf("本机限流不应计入上游熔断，CooldownUntil=%v", cd)
	}
	// 放行的 2 次均已成功完成 → 成功数窗口同样记录 2。
	if reqs, succs, _ := g.rl.WindowStats("g|"+testClientIP, time.Minute); reqs != 2 || succs != 2 {
		t.Fatalf("窗口统计应 reqs=2 succs=2，实际 %d / %d", reqs, succs)
	}
}

// TestGatewayGroupRateLimitOverridesGlobal 分组覆盖全局：default 组受全局
// count=1 约束；vip 组命中分组 JSON [5,0] 后 5 次全放行，且两组命名空间隔离。
func TestGatewayGroupRateLimitOverridesGlobal(t *testing.T) {
	g, st, plain := newTestGateway(t, map[string]string{
		OptionKeyRateLimitEnabled: "true",
		OptionKeyRateLimitCount:   "1",
		OptionKeyRateLimitGroup:   `{"vip":[5,0]}`,
	})
	up, _ := fakeUpstream(t, okUpstream())
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	w1 := postChat(t, g, plain, body)
	w2 := postChat(t, g, plain, body)
	if w1.Code != http.StatusOK || w2.Code != http.StatusTooManyRequests {
		t.Fatalf("default 组应受全局 count=1：第 1 次 200 / 第 2 次 429，实际 %d / %d", w1.Code, w2.Code)
	}

	vip := seedToken(t, st, func(tk *model.Token) { tk.Group = "vip" })
	for i := 0; i < 5; i++ {
		w := postChat(t, g, vip, body)
		if w.Code != http.StatusOK {
			t.Fatalf("vip 组分组覆盖 [5,0]：第 %d 次应放行，实际 %d", i+1, w.Code)
		}
	}
	if w := postChat(t, g, vip, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("vip 组第 6 次应 429，实际 %d", w.Code)
	}
	// 分组命中后 scope 隔离：全局窗口不应被 vip 流量污染；
	// 被拒请求不消耗配额（拒绝不记录），故 default 组窗口内仅 1 次放行。
	if reqs, _, _ := g.rl.WindowStats("g|"+testClientIP, time.Minute); reqs != 1 {
		t.Fatalf("全局 scope 应只含 default 组 1 次放行请求（被拒不计数），实际 %d", reqs)
	}
}

// TestGatewayTokenGroupGroupRatio 令牌分组接 GroupRatio（wave3 遗留项）：
// token.group=vip → 组倍率 2.0 参与计费（实扣 = 100 tokens × 0.5 × 2.0 × 0.5 = 50），
// 日志 detail.GroupRatio 落库 2.0。
func TestGatewayTokenGroupGroupRatio(t *testing.T) {
	g, st, _ := newTestGateway(t, map[string]string{
		"GroupRatio": `{"vip":2.0}`,
	})
	up, _ := fakeUpstream(t, okUpstream())
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	plain := seedToken(t, st, func(tk *model.Token) {
		tk.UnlimitedQuota = false
		tk.RemainQuota = 100000
		tk.Group = "vip"
	})
	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	tok := reloadToken(t, st, plain)
	// classic：q = (input + completion×comp) × modelRatio × groupRatio × 500000/1e6
	//        = 100 × 0.5 × 2.0 × 0.5 = 50。
	if want := int64(100000 - 50); tok.RemainQuota != want {
		t.Fatalf("分组倍率应参与计费：余额应 %d，实际 %d", want, tok.RemainQuota)
	}
	rows := logRowsAfterClose(t, g, st)
	if len(rows) == 0 {
		t.Fatal("应产生请求日志")
	}
	detail := decodeLogDetail(t, rows[len(rows)-1])
	if detail.GroupRatio != 2.0 {
		t.Fatalf("detail.GroupRatio 应落库 2.0，实际 %v", detail.GroupRatio)
	}
}

// TestGatewayTokenModelLimits 令牌模型白名单：请求模型不在清单内 → 403。
func TestGatewayTokenModelLimits(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	plain = seedToken(t, st, func(tk *model.Token) { tk.ModelLimits = "other" })

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[]}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("模型不在白名单应 403，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token_model_forbidden") {
		t.Fatalf("错误码应为 token_model_forbidden: %s", w.Body.String())
	}
}

// TestGatewayTokenAllowIPs 令牌 IP 白名单：CIDR 不命中 → 403；命中 → 正常转发。
func TestGatewayTokenAllowIPs(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, _ := fakeUpstream(t, okUpstream())
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})

	deny := seedToken(t, st, func(tk *model.Token) { tk.AllowIPs = "10.0.0.0/8" })
	w := postChat(t, g, deny, []byte(`{"model":"m1","messages":[]}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("IP 不在白名单应 403，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token_ip_forbidden") {
		t.Fatalf("错误码应为 token_ip_forbidden: %s", w.Body.String())
	}

	allow := seedToken(t, st, func(tk *model.Token) { tk.AllowIPs = "192.0.2.0/24" })
	w = postChat(t, g, allow, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("IP 命中白名单应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestGatewayTokenTPMRPM 令牌级 TPM/RPM：RPM=1 第 2 次 429；TPM=10 在首请求
// 消耗 100 tokens 后第 2 次 429，Retry-After 均指向窗口内。
func TestGatewayTokenTPMRPM(t *testing.T) {
	g, st, _ := newTestGateway(t, nil)
	up, hits := fakeUpstream(t, okUpstream())
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "k", Models: "m1"})
	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	rpm := seedToken(t, st, func(tk *model.Token) { tk.TPMRPM = `{"rpm":1}` })
	if w := postChat(t, g, rpm, body); w.Code != http.StatusOK {
		t.Fatalf("RPM 首次应放行，实际 %d", w.Code)
	}
	if w := postChat(t, g, rpm, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("RPM=1 第 2 次应 429，实际 %d body=%s", w.Code, w.Body.String())
	} else if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 1 || ra > 60 {
		t.Fatalf("RPM 429 Retry-After 应在 (0,60]，实际 %q err=%v", w.Header().Get("Retry-After"), err)
	}

	tpm := seedToken(t, st, func(tk *model.Token) { tk.TPMRPM = `{"tpm":10}` })
	if w := postChat(t, g, tpm, body); w.Code != http.StatusOK {
		t.Fatalf("TPM 首次应放行，实际 %d", w.Code)
	}
	// 首请求成功消耗 100 tokens ≥ TPM 10 → 第 2 次在限流层被拒（不触上游）。
	before := len(*hits)
	if w := postChat(t, g, tpm, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("TPM=10 累计 100 后第 2 次应 429，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(*hits) != before {
		t.Fatalf("TPM 限流应发生在渠道选择之前，上游请求数不应增加")
	}
}
