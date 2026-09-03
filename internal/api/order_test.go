// order_test.go 在线充值订单与回调结算测试（M3-wave3，docs/05 §5.10）：
// 覆盖下单校验矩阵（鉴权/网关门控/金额区间/配置完整性）、epay notify 验签
// 与金额篡改拒绝、12 并发通知幂等恰一次入账（含 aff 返利恰一次）、stripe
// webhook 三态与金额不符拒绝、订单归属越权、aff 信息端点与 return 重定向。
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/payment"
	"github.com/1923256780/hui-api/internal/store"
)

// enableEpay 打开 epay 网关并写入测试配置。
func enableEpay(t *testing.T, h *Handler) {
	t.Helper()
	setOpts(t, h, map[string]string{
		OptionKeyEpayEnabled:   "true",
		OptionKeyEpayGateway:   "https://pay.test",
		OptionKeyEpayPID:       "1001",
		OptionKeyEpaySecretKey: "epay-test-secret",
		OptionKeyEpayPayType:   "alipay",
	})
}

// orderNoSeq 订单号唯一化序号：Windows 时钟粒度（约 15.6ms）下连续 seed 的
// UnixNano 可能相同而撞 order_no 唯一索引（CI Linux 精度高未暴露），追加
// 原子序数保证恒唯一。
var orderNoSeq atomic.Int64

// seedTopupOrder 写入一笔指定状态的充值订单并返回。
func seedTopupOrder(t *testing.T, st *store.Store, uid int64, gateway, currency string, amountCents, quota, rate, status int64) model.TopupOrder {
	t.Helper()
	o := model.TopupOrder{
		OrderNo:     fmt.Sprintf("TPTEST%d-%d", time.Now().UnixNano(), orderNoSeq.Add(1)),
		UserID:      uid,
		Gateway:     gateway,
		AmountCents: amountCents,
		Currency:    currency,
		Quota:       quota,
		Rate:        rate,
		Status:      int(status),
		CreatedTime: time.Now().Unix(),
	}
	if err := st.Write.Create(&o).Error; err != nil {
		t.Fatalf("写入订单失败: %v", err)
	}
	return o
}

// epayNotifyQuery 构造带正确 MD5 签名的 notify 查询串。
func epayNotifyQuery(params map[string]string, secret string) string {
	vs := url.Values{}
	for k, v := range params {
		vs.Set(k, v)
	}
	vs.Set("sign_type", "MD5")
	vs.Set("sign", payment.EPaySign(params, secret))
	return vs.Encode()
}

// orderResp 是下单成功响应的 data 形状。
type orderResp struct {
	OrderNo string `json:"order_no"`
	PayURL  string `json:"pay_url"`
	Quota   int64  `json:"quota"`
}

// decodeOrderResp 解析下单响应。
func decodeOrderResp(t *testing.T, body []byte) orderResp {
	t.Helper()
	var data orderResp
	if err := json.Unmarshal(body, &struct {
		Data *orderResp `json:"data"`
	}{&data}); err != nil {
		t.Fatalf("解析下单响应失败: %v (%s)", err, body)
	}
	return data
}

// orderListResp 是订单列表响应的 data 形状。
type orderListResp struct {
	Items    []model.TopupOrder `json:"items"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

// affResp 是邀请信息响应的 data 形状。
type affResp struct {
	AffCode         string `json:"aff_code"`
	InvitedCount    int64  `json:"invited_count"`
	AffHistoryQuota int64  `json:"aff_history_quota"`
	RebatePercent   int64  `json:"rebate_percent"`
}

// topupOrderByNo 按 order_no 读订单。
func topupOrderByNo(t *testing.T, st *store.Store, orderNo string) model.TopupOrder {
	t.Helper()
	var o model.TopupOrder
	if err := st.Read.Where("order_no = ?", orderNo).First(&o).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	return o
}

// TestCreateTopupOrderValidation 下单校验矩阵：未登录/未知网关/未启用网关/
// 金额越界/配置不完整。
func TestCreateTopupOrderValidation(t *testing.T) {
	r, st, h := newTestAPI(t)
	seedUser(t, st, "alice", "pw-alice", 1)
	cookie := loginAndCookie(t, r, "alice", "pw-alice")

	// 未登录 401。
	if w := doJSON(t, r, http.MethodPost, "/api/user/topup/order", "",
		map[string]any{"gateway": "epay", "amount_cents": 1000}); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
	// 未知网关 400。
	w := doJSON(t, r, http.MethodPost, "/api/user/topup/order", cookie,
		map[string]any{"gateway": "paypal", "amount_cents": 1000})
	if w.Code != http.StatusBadRequest || respCode(t, w.Body.Bytes()) != "invalid_request" {
		t.Fatalf("未知网关应 400 invalid_request，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 网关未启用 403（epay 配置未开）。
	w = doJSON(t, r, http.MethodPost, "/api/user/topup/order", cookie,
		map[string]any{"gateway": "epay", "amount_cents": 1000})
	if w.Code != http.StatusForbidden || respCode(t, w.Body.Bytes()) != "gateway_disabled" {
		t.Fatalf("未启用网关应 403 gateway_disabled，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 开启后金额越界：低于默认 min（100 分）。
	enableEpay(t, h)
	w = doJSON(t, r, http.MethodPost, "/api/user/topup/order", cookie,
		map[string]any{"gateway": "epay", "amount_cents": 50})
	if w.Code != http.StatusBadRequest || respCode(t, w.Body.Bytes()) != "amount_out_of_range" {
		t.Fatalf("金额低于下限应 400 amount_out_of_range，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 超过 max（配置 1000 分）。
	setOpts(t, h, map[string]string{OptionKeyTopupMaxAmountCents: "1000"})
	w = doJSON(t, r, http.MethodPost, "/api/user/topup/order", cookie,
		map[string]any{"gateway": "epay", "amount_cents": 2000})
	if w.Code != http.StatusBadRequest || respCode(t, w.Body.Bytes()) != "amount_out_of_range" {
		t.Fatalf("金额超上限应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 配置不完整：全新实例只开开关、不写 pid/secret。
	r2, st2, h2 := newTestAPI(t)
	seedUser(t, st2, "bob", "pw-bob", 1)
	cookie2 := loginAndCookie(t, r2, "bob", "pw-bob")
	setOpts(t, h2, map[string]string{OptionKeyEpayEnabled: "true"})
	w2 := doJSON(t, r2, http.MethodPost, "/api/user/topup/order", cookie2,
		map[string]any{"gateway": "epay", "amount_cents": 1000})
	if w2.Code != http.StatusBadRequest || respCode(t, w2.Body.Bytes()) != "gateway_not_configured" {
		t.Fatalf("配置不完整应 400 gateway_not_configured，实际 %d body=%s", w2.Code, w2.Body.String())
	}
}

// TestCreateTopupOrderEpay epay 下单：跳转 URL 形态与签名正确性、订单快照
// （quota/rate/currency/状态）。
func TestCreateTopupOrderEpay(t *testing.T) {
	r, st, h := newTestAPI(t)
	u := seedUser(t, st, "alice", "pw-alice", 1)
	cookie := loginAndCookie(t, r, "alice", "pw-alice")
	enableEpay(t, h)

	w := doJSON(t, r, http.MethodPost, "/api/user/topup/order", cookie,
		map[string]any{"gateway": "epay", "amount_cents": 1000})
	if w.Code != http.StatusOK {
		t.Fatalf("下单应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	data := decodeOrderResp(t, w.Body.Bytes())
	// ¥10（1000 分）@720 定点 → 1000×500000/720 = 694444。
	if data.Quota != 694444 {
		t.Fatalf("换算额度应 694444，实际 %d", data.Quota)
	}
	u1, err := url.Parse(data.PayURL)
	if err != nil {
		t.Fatalf("pay_url 解析失败: %v", err)
	}
	if u1.Host != "pay.test" || u1.Path != "/submit.php" {
		t.Fatalf("跳转地址形态不符: %s", data.PayURL)
	}
	q := u1.Query()
	if q.Get("pid") != "1001" || q.Get("type") != "alipay" || q.Get("money") != "10.00" ||
		q.Get("out_trade_no") != data.OrderNo || q.Get("sign_type") != "MD5" || q.Get("sign") == "" {
		t.Fatalf("跳转参数不符: %v", q)
	}
	// 签名可被同一密钥重算验证（URL 全参数闭环）。
	p2 := map[string]string{}
	for k := range q {
		if k != "sign" && k != "sign_type" {
			p2[k] = q.Get(k)
		}
	}
	if payment.EPaySign(p2, "epay-test-secret") != q.Get("sign") {
		t.Fatal("pay_url 签名应与参数+商户密钥一致")
	}
	// 订单快照。
	o := topupOrderByNo(t, st, data.OrderNo)
	if o.Status != model.TopupOrderPending || o.UserID != u.ID ||
		o.AmountCents != 1000 || o.Quota != 694444 || o.Rate != 720 || o.Currency != "CNY" {
		t.Fatalf("订单快照不符: %+v", o)
	}
}

// TestCreateTopupOrderStripe stripe 下单：假端点捕获请求（metadata.order_no、
// 金额、success_url），订单快照（USD 本位 rate=100、trade_no=session id）。
func TestCreateTopupOrderStripe(t *testing.T) {
	r, st, h := newTestAPI(t)
	seedUser(t, st, "alice", "pw-alice", 1)
	cookie := loginAndCookie(t, r, "alice", "pw-alice")

	var captured url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		captured, _ = url.ParseQuery(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.test/pay/cs_test_1"}`))
	}))
	defer srv.Close()
	h.stripeHTTP = srv.Client()
	h.stripeAPIBase = srv.URL
	setOpts(t, h, map[string]string{
		OptionKeyStripeEnabled:   "true",
		OptionKeyStripeSecretKey: "sk_test_x",
	})

	w := doJSON(t, r, http.MethodPost, "/api/user/topup/order", cookie,
		map[string]any{"gateway": "stripe", "amount_cents": 1000})
	if w.Code != http.StatusOK {
		t.Fatalf("下单应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	data := decodeOrderResp(t, w.Body.Bytes())
	// $10（1000 美分）USD 本位 → 1000×5000 = 5000000。
	if data.Quota != 5000000 {
		t.Fatalf("换算额度应 5000000，实际 %d", data.Quota)
	}
	if data.PayURL != "https://checkout.test/pay/cs_test_1" {
		t.Fatalf("应返回收银台地址，实际 %s", data.PayURL)
	}
	if got := captured.Get("metadata[order_no]"); got != data.OrderNo {
		t.Fatalf("metadata.order_no 应回传本地单号: %q", got)
	}
	if got := captured.Get("line_items[0][price_data][unit_amount]"); got != "1000" {
		t.Fatalf("unit_amount 应为 1000，实际 %q", got)
	}
	if !strings.Contains(captured.Get("success_url"), "order="+data.OrderNo) {
		t.Fatalf("success_url 应携带订单号: %q", captured.Get("success_url"))
	}
	o := topupOrderByNo(t, st, data.OrderNo)
	if o.Status != model.TopupOrderPending || o.Currency != "USD" || o.Rate != 100 ||
		o.TradeNo != "cs_test_1" || o.AmountCents != 1000 {
		t.Fatalf("订单快照不符: %+v", o)
	}
}

// TestEpayNotifyIdempotentConcurrent 12 并发重复通知同一订单：全部应答
// success（幂等）、买家恰一次入账、topup 日志恰一条、aff 返利恰一次。
func TestEpayNotifyIdempotentConcurrent(t *testing.T) {
	r, st, h := newTestAPI(t)
	u := seedUser(t, st, "alice", "pw-alice", 1)
	inviter := seedUser(t, st, "boss", "pw-boss", 1)
	if err := st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Update("inviter_id", inviter.ID).Error; err != nil {
		t.Fatalf("建立邀请关系失败: %v", err)
	}
	enableEpay(t, h)
	setOpts(t, h, map[string]string{OptionKeyAffRebatePercent: "10"})

	// ¥10 单：quota=694444，返利 round(694444×10/100)=69444。
	o := seedTopupOrder(t, st, u.ID, "epay", "CNY", 1000, 694444, 720, model.TopupOrderPending)
	params := map[string]string{
		"pid":          "1001",
		"type":         "alipay",
		"out_trade_no": o.OrderNo,
		"trade_no":     "20260903001",
		"trade_status": "TRADE_SUCCESS",
		"money":        "10.00",
		"name":         "余额充值",
	}
	path := "/api/pay/epay/notify?" + epayNotifyQuery(params, "epay-test-secret")

	const n = 12
	start := make(chan struct{})
	codes := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // 全员就位后同时发起重复通知
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			codes[idx] = w.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != "success" {
			t.Fatalf("第 %d 个重复通知应幂等回 success，实际 %q（全部: %v）", i, c, codes)
		}
	}
	if got := userQuota(t, st, u.ID); got != 694444 {
		t.Fatalf("并发重复通知应恰一次入账（694444），实际 %d", got)
	}
	o2 := topupOrderByNo(t, st, o.OrderNo)
	if o2.Status != model.TopupOrderPaid || o2.TradeNo != "20260903001" || o2.PaidTime == 0 {
		t.Fatalf("订单应已结算: %+v", o2)
	}
	var logs []model.Log
	if err := st.Read.Where("protocol = ? AND model_name = ?", "topup", "order_paid").
		Find(&logs).Error; err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if len(logs) != 1 || logs[0].UserID != u.ID || logs[0].Quota != 694444 {
		t.Fatalf("topup 日志应恰一条: %+v", logs)
	}
	// aff 返利恰一次：694444×10% → 69444。
	if got := userQuota(t, st, inviter.ID); got != 69444 {
		t.Fatalf("邀请人应恰一次返利 69444，实际 %d", got)
	}
	var iv model.User
	if err := st.Read.First(&iv, inviter.ID).Error; err != nil {
		t.Fatalf("读取邀请人失败: %v", err)
	}
	if iv.AffHistoryQuota != 69444 {
		t.Fatalf("aff_history_quota 应 69444，实际 %d", iv.AffHistoryQuota)
	}
	var affLogs []model.Log
	if err := st.Read.Where("protocol = ? AND model_name = ?", "aff", "topup_rebate").
		Find(&affLogs).Error; err != nil {
		t.Fatalf("查询返利日志失败: %v", err)
	}
	if len(affLogs) != 1 || affLogs[0].UserID != inviter.ID || affLogs[0].Quota != 69444 {
		t.Fatalf("返利日志应恰一条: %+v", affLogs)
	}
}

// TestEpayNotifyRejections notify 拒绝路径：验签失败/缺单号/金额篡改/网关
// 不匹配/订单不存在均 fail 且无任何入账；非成功 trade_status 确认但不动账。
func TestEpayNotifyRejections(t *testing.T) {
	r, st, h := newTestAPI(t)
	u := seedUser(t, st, "alice", "pw-alice", 1)
	enableEpay(t, h)
	o := seedTopupOrder(t, st, u.ID, "epay", "CNY", 1000, 694444, 720, model.TopupOrderPending)
	stripeOrder := seedTopupOrder(t, st, u.ID, "stripe", "USD", 1000, 5000000, 100, model.TopupOrderPending)

	base := map[string]string{
		"pid": "1001", "type": "alipay", "trade_no": "T1",
		"trade_status": "TRADE_SUCCESS", "money": "10.00", "name": "余额充值",
	}
	doNotify := func(params map[string]string, secret string) string {
		req := httptest.NewRequest(http.MethodGet, "/api/pay/epay/notify?"+epayNotifyQuery(params, secret), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Body.String()
	}
	expectPending := func(orderNo string) {
		t.Helper()
		if o := topupOrderByNo(t, st, orderNo); o.Status != model.TopupOrderPending {
			t.Fatalf("订单应保持 pending: %+v", o)
		}
	}

	// 正确签名（基准组）→ success。
	okParams := cloneStrMap(base)
	okParams["out_trade_no"] = o.OrderNo
	if got := doNotify(okParams, "epay-test-secret"); got != "success" {
		t.Fatalf("正确通知应 success，实际 %q", got)
	}
	if got := userQuota(t, st, u.ID); got != 694444 {
		t.Fatalf("基准组应入账 694444，实际 %d", got)
	}

	// 验签失败：篡改金额后原签名 → fail。
	tampered := cloneStrMap(base)
	tampered["out_trade_no"] = o.OrderNo
	tampered["money"] = "0.01"
	if got := doNotify(tampered, "epay-test-secret"); got != "fail" {
		t.Fatalf("金额篡改通知应 fail，实际 %q", got)
	}
	// 错误商户密钥 → fail。
	if got := doNotify(okParams, "wrong-secret"); got != "fail" {
		t.Fatalf("错误密钥应 fail，实际 %q", got)
	}
	// 缺 out_trade_no → fail。
	noNo := cloneStrMap(base)
	delete(noNo, "out_trade_no")
	if got := doNotify(noNo, "epay-test-secret"); got != "fail" {
		t.Fatalf("缺单号应 fail，实际 %q", got)
	}
	// 订单不存在 → fail。
	unknown := cloneStrMap(base)
	unknown["out_trade_no"] = "TPNOTEXIST"
	if got := doNotify(unknown, "epay-test-secret"); got != "fail" {
		t.Fatalf("未知订单应 fail，实际 %q", got)
	}
	// 网关不匹配（stripe 单收到 epay 通知）→ fail。
	cross := cloneStrMap(base)
	cross["out_trade_no"] = stripeOrder.OrderNo
	if got := doNotify(cross, "epay-test-secret"); got != "fail" {
		t.Fatalf("跨网关通知应 fail，实际 %q", got)
	}
	// 金额不符（签名正确但金额与订单不一致）→ fail。
	wrongAmt := cloneStrMap(base)
	wrongAmt["out_trade_no"] = o.OrderNo
	wrongAmt["money"] = "9.99"
	// 注意 o 已结算，重复通知本应 success（幂等），但金额不符优先拒绝：
	// 用第二笔 pending 单验证金额校验。
	o2 := seedTopupOrder(t, st, u.ID, "epay", "CNY", 2000, 1388889, 720, model.TopupOrderPending)
	wrongAmt["out_trade_no"] = o2.OrderNo
	if got := doNotify(wrongAmt, "epay-test-secret"); got != "fail" {
		t.Fatalf("金额不符应 fail，实际 %q", got)
	}
	expectPending(o2.OrderNo)
	// 非成功 trade_status：确认但不入账。
	closed := cloneStrMap(base)
	closed["out_trade_no"] = o2.OrderNo
	closed["trade_status"] = "TRADE_CLOSED"
	if got := doNotify(closed, "epay-test-secret"); got != "success" {
		t.Fatalf("非成功态应确认 success，实际 %q", got)
	}
	expectPending(o2.OrderNo)
	if got := userQuota(t, st, u.ID); got != 694444 {
		t.Fatalf("拒绝路径不应产生任何额外入账，余额 %d", got)
	}
}

// cloneStrMap 复制 map（测试篡改不改原参数）。
func cloneStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// stripeEventBody 构造 webhook 事件 JSON。
func stripeEventBody(typ, orderNo string, amountTotal int64) string {
	obj := map[string]any{
		"id": "cs_evt_1", "amount_total": amountTotal, "currency": "usd",
		"metadata": map[string]string{"order_no": orderNo},
	}
	b, _ := json.Marshal(map[string]any{
		"type": typ, "data": map[string]any{"object": obj},
	})
	return string(b)
}

// stripeSigHeader 以给定时间戳与密钥构造合法 Stripe-Signature 头。
func stripeSigHeader(secret string, ts int64, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + payload))
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// postWebhook 发起 webhook 请求。
func postWebhook(r *gin.Engine, body, sigHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/pay/stripe/webhook", strings.NewReader(body))
	if sigHeader != "" {
		req.Header.Set("Stripe-Signature", sigHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestStripeWebhookStates webhook 三态与边界：正确入账、错误签名 400、
// 过期时间戳 400、非 completed 事件忽略、订单不存在 404、金额不符 400。
func TestStripeWebhookStates(t *testing.T) {
	r, st, h := newTestAPI(t)
	u := seedUser(t, st, "alice", "pw-alice", 1)
	const secret = "whsec-test"
	setOpts(t, h, map[string]string{OptionKeyStripeWebhookSecret: secret})
	o := seedTopupOrder(t, st, u.ID, "stripe", "USD", 1000, 5000000, 100, model.TopupOrderPending)
	o2 := seedTopupOrder(t, st, u.ID, "stripe", "USD", 2000, 10000000, 100, model.TopupOrderPending)

	now := time.Now().Unix()
	body := stripeEventBody("checkout.session.completed", o.OrderNo, 1000)

	// 正确签名 → 200、入账。
	if w := postWebhook(r, body, stripeSigHeader(secret, now, body)); w.Code != http.StatusOK {
		t.Fatalf("正确 webhook 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if got := userQuota(t, st, u.ID); got != 5000000 {
		t.Fatalf("webhook 入账应 5000000，实际 %d", got)
	}
	// 错误签名 → 400。
	if w := postWebhook(r, body, stripeSigHeader(secret, now, "tampered-body")); w.Code != http.StatusBadRequest {
		t.Fatalf("错误签名应 400，实际 %d", w.Code)
	}
	// 过期时间戳（±5min 容差外）→ 400。
	if w := postWebhook(r, body, stripeSigHeader(secret, now-360, body)); w.Code != http.StatusBadRequest {
		t.Fatalf("过期时间戳应 400，实际 %d", w.Code)
	}
	// 非 completed 事件 → 200 忽略、不动账。
	other := stripeEventBody("charge.refunded", o.OrderNo, 1000)
	if w := postWebhook(r, other, stripeSigHeader(secret, now, other)); w.Code != http.StatusOK {
		t.Fatalf("非 completed 事件应 200 忽略，实际 %d", w.Code)
	}
	// 订单不存在 → 404。
	missing := stripeEventBody("checkout.session.completed", "TPNONE", 1000)
	if w := postWebhook(r, missing, stripeSigHeader(secret, now, missing)); w.Code != http.StatusNotFound {
		t.Fatalf("订单不存在应 404，实际 %d", w.Code)
	}
	// 金额不符 → 400 且订单保持 pending。
	mismatch := stripeEventBody("checkout.session.completed", o2.OrderNo, 999)
	if w := postWebhook(r, mismatch, stripeSigHeader(secret, now, mismatch)); w.Code != http.StatusBadRequest {
		t.Fatalf("金额不符应 400，实际 %d", w.Code)
	}
	if got := topupOrderByNo(t, st, o2.OrderNo); got.Status != model.TopupOrderPending {
		t.Fatalf("金额不符订单应保持 pending: %+v", got)
	}
	// 幂等重放同一正确通知 → 200 且不重复入账。
	if w := postWebhook(r, body, stripeSigHeader(secret, now, body)); w.Code != http.StatusOK {
		t.Fatalf("幂等重放应 200，实际 %d", w.Code)
	}
	if got := userQuota(t, st, u.ID); got != 5000000 {
		t.Fatalf("幂等重放不应重复入账，余额 %d", got)
	}
}

// TestListMyTopupOrdersOwnership 订单列表本人作用域与分页：A 只见 A 的单。
func TestListMyTopupOrdersOwnership(t *testing.T) {
	r, st, _ := newTestAPI(t)
	a := seedUser(t, st, "alice", "pw-alice", 1)
	b := seedUser(t, st, "bob", "pw-bob", 1)
	seedTopupOrder(t, st, a.ID, "epay", "CNY", 100, 69444, 720, model.TopupOrderPending)
	seedTopupOrder(t, st, a.ID, "epay", "CNY", 200, 138889, 720, model.TopupOrderPaid)
	seedTopupOrder(t, st, b.ID, "stripe", "USD", 100, 500000, 100, model.TopupOrderPending)
	cookieA := loginAndCookie(t, r, "alice", "pw-alice")
	cookieB := loginAndCookie(t, r, "bob", "pw-bob")

	var page orderListResp
	w := doJSON(t, r, http.MethodGet, "/api/user/topup/orders", cookieA, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &struct {
		Data *orderListResp `json:"data"`
	}{&page}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("A 应只见自己的 2 单，total=%d items=%d", page.Total, len(page.Items))
	}
	for _, it := range page.Items {
		if it.UserID != a.ID {
			t.Fatalf("列表混入他人订单: %+v", it)
		}
	}
	// B 只有 1 单。
	wB := doJSON(t, r, http.MethodGet, "/api/user/topup/orders", cookieB, nil)
	if err := json.Unmarshal(wB.Body.Bytes(), &struct {
		Data *orderListResp `json:"data"`
	}{&page}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("B 应只见自己的 1 单，total=%d", page.Total)
	}
	// 分页：page_size=1。
	wP := doJSON(t, r, http.MethodGet, "/api/user/topup/orders?page=1&page_size=1", cookieA, nil)
	if err := json.Unmarshal(wP.Body.Bytes(), &struct {
		Data *orderListResp `json:"data"`
	}{&page}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 1 || page.PageSize != 1 {
		t.Fatalf("分页不符: total=%d items=%d size=%d", page.Total, len(page.Items), page.PageSize)
	}
	// 未登录 401。
	if w := doJSON(t, r, http.MethodGet, "/api/user/topup/orders", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
}

// TestGetMyAff 邀请信息端点：aff_code 惰性补发、邀请人数与累计返利。
func TestGetMyAff(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "alice", "pw-alice", 1) // aff_code 为空 → 惰性补发
	b1 := seedUser(t, st, "inv1", "pw-inv1", 1)
	b2 := seedUser(t, st, "inv2", "pw-inv2", 1)
	for _, b := range []model.User{b1, b2} {
		if err := st.Write.Model(&model.User{}).Where("id = ?", b.ID).
			Update("inviter_id", u.ID).Error; err != nil {
			t.Fatalf("建立邀请关系失败: %v", err)
		}
	}
	if err := st.Write.Model(&model.User{}).Where("id = ?", u.ID).
		Update("aff_history_quota", 12345).Error; err != nil {
		t.Fatalf("设置累计返利失败: %v", err)
	}
	cookie := loginAndCookie(t, r, "alice", "pw-alice")

	var data affResp
	w := doJSON(t, r, http.MethodGet, "/api/user/aff", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("aff 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &struct {
		Data *affResp `json:"data"`
	}{&data}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if data.AffCode == "" || len(data.AffCode) != 8 {
		t.Fatalf("aff_code 应惰性补发 8 位码: %q", data.AffCode)
	}
	if data.InvitedCount != 2 || data.AffHistoryQuota != 12345 {
		t.Fatalf("邀请统计不符: %+v", data)
	}
	// 二次访问复用同一码（不重新生成）。
	w2 := doJSON(t, r, http.MethodGet, "/api/user/aff", cookie, nil)
	var data2 affResp
	if err := json.Unmarshal(w2.Body.Bytes(), &struct {
		Data *affResp `json:"data"`
	}{&data2}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if data2.AffCode != data.AffCode {
		t.Fatalf("aff_code 应稳定: %q vs %q", data2.AffCode, data.AffCode)
	}
	// 未登录 401。
	if w := doJSON(t, r, http.MethodGet, "/api/user/aff", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
}

// TestEpayReturnRedirect return 回跳 302 到控制台并携带订单号。
func TestEpayReturnRedirect(t *testing.T) {
	r, _, _ := newTestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/pay/epay/return?out_trade_no=TP123&sign=x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("return 应 302，实际 %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/console/topup?order=TP123" {
		t.Fatalf("应重定向到控制台充值页: %q", loc)
	}
}
