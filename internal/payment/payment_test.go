// payment_test.go：支付适配层单元测试（M3-wave3，docs/05 §5.10）。
//
// 覆盖：EPay 签名串固定格式（字典序、k=v& 带尾 &、末尾接 key、空值与
// sign/sign_type 排除）、MD5 hex 小写、notify 验签正/篡改/缺 sign 三态、
// SubmitURL 构造（尾斜杠归一 + URL 编码）；Stripe Webhook 验签
// 正/篡改/过期三态与畸形头拒绝、Checkout Session 请求形态断言
// （Basic 鉴权、form 字段、metadata.order_no 回传）与错误路径。
package payment

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// epaySampleParams 构造一份含干扰项的典型下单参数（空值与 sign/sign_type
// 应被签名逻辑排除）。
func epaySampleParams() map[string]string {
	return map[string]string{
		"pid":          "1001",
		"type":         "alipay",
		"out_trade_no": "R20260903001",
		"notify_url":   "https://pay.example/api/pay/epay/notify",
		"return_url":   "https://pay.example/api/pay/epay/return",
		"name":         "Balance",
		"money":        "10.00",
		"body":         "", // 空值不参与签名
		"sign":         "ignored_sign",
		"sign_type":    "MD5",
	}
}

// TestEPaySignBaseGolden 固定向量：期望签名串在测试内独立构造（不调用被测
// 函数），双向互证排序/过滤/拼接格式。字典序：
// money,name,notify_url,out_trade_no,pid,return_url,type。
func TestEPaySignBaseGolden(t *testing.T) {
	params := epaySampleParams()
	const key = "testkey-123"
	want := "money=10.00&name=Balance&notify_url=https://pay.example/api/pay/epay/notify" +
		"&out_trade_no=R20260903001&pid=1001&return_url=https://pay.example/api/pay/epay/return" +
		"&type=alipay&" + key
	if got := epaySignBase(params, key); got != want {
		t.Fatalf("签名串格式不符:\n got  = %s\n want = %s", got, want)
	}
}

// TestEPaySignProperties 签名性质：hex 小写、输入顺序无关（map 随机遍历）、
// key 参与混淆、空值/被排除参数不影响结果。
func TestEPaySignProperties(t *testing.T) {
	params := epaySampleParams()
	const key = "testkey-123"
	got := EPaySign(params, key)
	if got != strings.ToLower(got) || len(got) != 32 {
		t.Fatalf("签名应为 32 位小写 hex: %q", got)
	}
	// 与独立重算互证（同一向量换一梱入口）。
	sum := md5.Sum([]byte(epaySignBase(params, key)))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("EPaySign 与签名串直算不一致: got=%s want=%s", got, want)
	}
	// key 变化 → 签名变化。
	if EPaySign(params, "other-key") == got {
		t.Fatal("不同 key 应产生不同签名")
	}
	// 空值参数、sign/sign_type 的存在与否不影响签名。
	trimmed := map[string]string{
		"pid": "1001", "type": "alipay", "out_trade_no": "R20260903001",
		"notify_url": "https://pay.example/api/pay/epay/notify",
		"return_url": "https://pay.example/api/pay/epay/return",
		"name":       "Balance", "money": "10.00",
	}
	if EPaySign(trimmed, key) != got {
		t.Fatal("空值与 sign/sign_type 参数不应影响签名结果")
	}
}

// TestEPayNotifyVerify 验签三态：正确 sign 通过；篡改任一参与参数拒绝；
// 缺 sign 拒绝；大写 hex sign 兼容通过。
func TestEPayNotifyVerify(t *testing.T) {
	const key = "notify-secret"
	params := epaySampleParams()
	delete(params, "sign")
	delete(params, "sign_type")
	params["sign_type"] = "MD5"
	params["sign"] = EPaySign(params, key)

	if !EPayNotifyVerify(params, key) {
		t.Fatal("正确签名应通过验签")
	}
	// 大写 hex 兼容。
	tampered := cloneParams(params)
	tampered["sign"] = strings.ToUpper(tampered["sign"])
	if !EPayNotifyVerify(tampered, key) {
		t.Fatal("大写 hex sign 应兼容通过")
	}
	// 篡改金额（攻击者改钱不改签）→ 拒绝。
	tampered = cloneParams(params)
	tampered["money"] = "0.01"
	if EPayNotifyVerify(tampered, key) {
		t.Fatal("篡改参数后验签必须失败")
	}
	// 篡改 out_trade_no（冒用他单）→ 拒绝。
	tampered = cloneParams(params)
	tampered["out_trade_no"] = "R20260903002"
	if EPayNotifyVerify(tampered, key) {
		t.Fatal("篡改 out_trade_no 后验签必须失败")
	}
	// 错误商户密钥 → 拒绝。
	if EPayNotifyVerify(params, "wrong-key") {
		t.Fatal("错误 key 验签必须失败")
	}
	// 缺 sign → 拒绝。
	noSign := cloneParams(params)
	delete(noSign, "sign")
	if EPayNotifyVerify(noSign, key) {
		t.Fatal("缺失 sign 必须拒绝")
	}
	// 空 sign → 拒绝。
	emptySign := cloneParams(params)
	emptySign["sign"] = ""
	if EPayNotifyVerify(emptySign, key) {
		t.Fatal("空 sign 必须拒绝")
	}
}

// TestEPaySubmitURL 跳转地址构造：尾斜杠归一、参数 URL 编码（空格 → +）、
// 携带 sign_type=MD5 与 sign 且 sign 与 EPaySign 同参一致。
func TestEPaySubmitURL(t *testing.T) {
	params := map[string]string{
		"pid": "1001", "type": "wxpay", "out_trade_no": "R1",
		"name": "Hello World", "money": "10.00",
	}
	const key = "submit-key"
	got := EPaySubmitURL("http://gw.example/", params, key)
	if !strings.HasPrefix(got, "http://gw.example/submit.php?") {
		t.Fatalf("网关尾斜杠应归一: %s", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("URL 解析失败: %v", err)
	}
	q := u.Query()
	if q.Get("name") != "Hello World" {
		t.Fatalf("参数应正确编码与解码: name=%q", q.Get("name"))
	}
	if q.Get("sign_type") != "MD5" || q.Get("sign") != EPaySign(params, key) {
		t.Fatalf("应携带 sign_type=MD5 与一致 sign: %v", q)
	}
	// 键序为字典序（url.Values.Encode 保证，这里复核首个键）。
	if first := strings.SplitN(u.RawQuery, "=", 2)[0]; first != "money" {
		t.Fatalf("query 应按参数名字典序: first=%s", first)
	}
}

// stripeSigHeader 构造合法 Stripe-Signature 头。
func stripeSigHeader(t *testing.T, ts int64, payload, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + payload))
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookVerify 三态与边界：正/篡改/过期、容差边界内通过、未来时间戳
// 超容差拒绝、多 v1 任一匹配、畸形头拒绝。
func TestWebhookVerify(t *testing.T) {
	const secret = "whsec_abc"
	payload := `{"type":"checkout.session.completed"}`
	now := time.Unix(1780000000, 0)

	header := stripeSigHeader(t, now.Unix(), payload, secret)
	if !WebhookVerify(header, payload, secret, now, 0) {
		t.Fatal("正确签名应通过")
	}
	if WebhookVerify(header, `{"type":"checkout.session.async_failed"}`, secret, now, 0) {
		t.Fatal("payload 篡改必须拒绝")
	}
	if WebhookVerify(header, payload, "whsec_other", now, 0) {
		t.Fatal("secret 不符必须拒绝")
	}
	// 过期（> ±5min）：旧/未来时间戳均拒绝；容差边界内通过。
	if WebhookVerify(stripeSigHeader(t, now.Add(-6*time.Minute).Unix(), payload, secret), payload, secret, now, 0) {
		t.Fatal("超容差旧时间戳必须拒绝（防重放）")
	}
	if WebhookVerify(stripeSigHeader(t, now.Add(6*time.Minute).Unix(), payload, secret), payload, secret, now, 0) {
		t.Fatal("超容差未来时间戳必须拒绝")
	}
	if !WebhookVerify(stripeSigHeader(t, now.Add(-4*time.Minute).Unix(), payload, secret), payload, secret, now, 0) {
		t.Fatal("容差内（-4min）应通过")
	}
	if !WebhookVerify(stripeSigHeader(t, now.Add(5*time.Minute).Unix(), payload, secret), payload, secret, now, 0) {
		t.Fatal("容差边界（+5min）应通过")
	}
	// 多 v1（旧签名失效后新签名追加）：任一匹配即通过。
	multi := header + "," + stripeSigHeader(t, now.Add(-time.Minute).Unix(), payload, secret)
	if !WebhookVerify(multi, payload, secret, now, 0) {
		t.Fatal("多 v1 任一匹配应通过")
	}
	// 畸形头：无 t / 无 v1 / t 非数字 / v1 非 hex。
	for _, bad := range []string{
		"v1=" + strings.Repeat("a", 64),
		"t=" + strconv.FormatInt(now.Unix(), 10),
		"t=notanumber,v1=" + strings.Repeat("a", 64),
		"t=" + strconv.FormatInt(now.Unix(), 10) + ",v1=zzzz",
	} {
		if WebhookVerify(bad, payload, secret, now, 0) {
			t.Fatalf("畸形头必须拒绝: %q", bad)
		}
	}
	// 空头。
	if WebhookVerify("", payload, secret, now, 0) {
		t.Fatal("空签名头必须拒绝")
	}
}

// TestCreateCheckoutSession 断言出站请求形态（POST 路径、Basic 鉴权、form
// 字段与 metadata.order_no）并解析假端点响应；附带参数校验与错误路径。
func TestCreateCheckoutSession(t *testing.T) {
	var gotUser, gotPass string
	var gotAuthOK bool
	var gotCT string
	var gotForm url.Values
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotUser, gotPass, gotAuthOK = r.BasicAuth()
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.example/pay/cs_test_1"}`))
	}))
	defer srv.Close()

	session, err := CreateCheckoutSession(context.Background(), srv.Client(), CheckoutSessionParams{
		APIBase: srv.URL, SecretKey: "sk_test_123",
		OrderNo: "R20260903001", AmountCents: 12345,
		Currency: "usd", ProductName: "Balance Top-up",
		SuccessURL: "https://app.example/console/topup?order=R20260903001&session_id={CHECKOUT_SESSION_ID}",
		CancelURL:  "https://app.example/console/topup?order=R20260903001",
	})
	if err != nil {
		t.Fatalf("创建 session 失败: %v", err)
	}
	if session.ID != "cs_test_1" || session.URL != "https://checkout.example/pay/cs_test_1" {
		t.Fatalf("响应解析不符: %+v", session)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/checkout/sessions" {
		t.Fatalf("请求形态不符: %s %s", gotMethod, gotPath)
	}
	// Basic 鉴权：secret_key 作用户名、密码空（Stripe 约定）。
	if !gotAuthOK || gotUser != "sk_test_123" || gotPass != "" {
		t.Fatalf("Basic 鉴权应为 (secret_key, 空密码): user=%q pass=%q ok=%v", gotUser, gotPass, gotAuthOK)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Fatalf("应 form 编码: %s", gotCT)
	}
	for _, want := range [][2]string{
		{"mode", "payment"},
		{"line_items[0][quantity]", "1"},
		{"line_items[0][price_data][currency]", "usd"},
		{"line_items[0][price_data][unit_amount]", "12345"},
		{"line_items[0][price_data][product_data][name]", "Balance Top-up"},
		{"metadata[order_no]", "R20260903001"},
		{"success_url", "https://app.example/console/topup?order=R20260903001&session_id={CHECKOUT_SESSION_ID}"},
		{"cancel_url", "https://app.example/console/topup?order=R20260903001"},
	} {
		if gotForm.Get(want[0]) != want[1] {
			t.Fatalf("form 字段 %s = %q, want %q", want[0], gotForm.Get(want[0]), want[1])
		}
	}

	// 参数校验与错误路径。
	if _, err := CreateCheckoutSession(context.Background(), srv.Client(), CheckoutSessionParams{APIBase: srv.URL}); err == nil {
		t.Fatal("缺 secret_key 应报错")
	}
	if _, err := CreateCheckoutSession(context.Background(), srv.Client(), CheckoutSessionParams{SecretKey: "sk", AmountCents: 0}); err == nil {
		t.Fatal("金额必须为正")
	}
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Invalid API key"}}`, http.StatusUnauthorized)
	}))
	defer errSrv.Close()
	if _, err := CreateCheckoutSession(context.Background(), errSrv.Client(), CheckoutSessionParams{
		APIBase: errSrv.URL, SecretKey: "bad", AmountCents: 100,
	}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("非 200 应报错且含状态码: %v", err)
	}
	noURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"cs_no_url"}`))
	}))
	defer noURL.Close()
	if _, err := CreateCheckoutSession(context.Background(), noURL.Client(), CheckoutSessionParams{
		APIBase: noURL.URL, SecretKey: "sk", AmountCents: 100,
	}); err == nil {
		t.Fatal("响应缺 url 应报错")
	}
}

// cloneParams 复制 map（测试篡改不改原参数）。
func cloneParams(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
