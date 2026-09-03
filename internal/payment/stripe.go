// stripe.go：Stripe Checkout Session 创建与 Webhook 签名校验（M3-wave3）。
// 刻意不引官方 SDK：出站面只有「创建 session 一个 POST」与「验签一个纯函数」，
// form 编码 + Basic 鉴权即可覆盖；零第三方依赖换最小攻击面与可测试性
// （出站经注入的 *http.Client，测试用 httptest 假端点）。
package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultStripeAPIBase 生产 Checkout REST 端点基地址（测试注入 httptest 假端点）。
const DefaultStripeAPIBase = "https://api.stripe.com"

// DefaultWebhookTolerance webhook 时间戳容差：±5 分钟（防重放窗口）。
const DefaultWebhookTolerance = 5 * time.Minute

// CheckoutSessionParams 创建 Checkout Session 的入参。
// SSRF 信任边界注记：APIBase 仅接受部署方注入（生产固定 DefaultStripeAPIBase，
// 测试注入 httptest 假端点），不读任何用户输入；与 oauth.go 的 issuer 配置
// 同一信任层级（root 管理员 options），部署义务注记 docs/11。
type CheckoutSessionParams struct {
	APIBase     string // 空则 DefaultStripeAPIBase
	SecretKey   string // sk_live_.../sk_test_...，Basic 鉴权用户名
	OrderNo     string // metadata[order_no]：webhook 回传关联回本地订单的唯一键
	AmountCents int64  // 最小计费单位（USD 分），必须 > 0
	Currency    string // 三字母小写，空则 usd
	ProductName string // 收银台展示名
	SuccessURL  string // 支付完成跳转（可含 {CHECKOUT_SESSION_ID} 占位）
	CancelURL   string // 取消跳转
}

// CheckoutSession 从创建响应中提取的业务字段。
type CheckoutSession struct {
	ID  string `json:"id"`  // cs_...（对账 / success_url 占位）
	URL string `json:"url"` // 收银台托管页，用户重定向目标
}

// CreateCheckoutSession POST {base}/v1/checkout/sessions 创建托管收银台
// session：form 编码 + Basic（secret_key 作用户名、密码留空，Stripe 约定）。
// line_items 用 price_data 内联自定义金额（免预建 price 对象）；
// metadata[order_no] 随 checkout.session.completed 事件原样回传。
func CreateCheckoutSession(ctx context.Context, client *http.Client, p CheckoutSessionParams) (*CheckoutSession, error) {
	if p.SecretKey == "" {
		return nil, errors.New("stripe: 缺少 secret_key")
	}
	if p.AmountCents <= 0 {
		return nil, errors.New("stripe: 金额必须为正")
	}
	if p.Currency == "" {
		p.Currency = "usd"
	}
	base := p.APIBase
	if base == "" {
		base = DefaultStripeAPIBase
	}
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", p.Currency)
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(p.AmountCents, 10))
	form.Set("line_items[0][price_data][product_data][name]", p.ProductName)
	form.Set("metadata[order_no]", p.OrderNo)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("stripe: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.SecretKey, "") // Stripe 约定：secret key 作用户名，密码留空
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("stripe: 读取响应: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stripe: 创建失败 HTTP %d: %s", resp.StatusCode, truncateForLog(body, 256))
	}
	var session CheckoutSession
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("stripe: 解析响应: %w", err)
	}
	if session.URL == "" {
		return nil, errors.New("stripe: 响应缺少 url 字段")
	}
	return &session, nil
}

// WebhookVerify 校验 Stripe-Signature 头（t=<unix秒>,v1=<hex>）：重算
// HMAC-SHA256(secret, "{t}.{payload}")，与头中每个 v1 恒时比较（hmac.Equal，
// 任一匹配即通过），且 |now - t| 不得超过 tolerance（防重放）。
// tolerance <= 0 时取 DefaultWebhookTolerance（±5 分钟）。
func WebhookVerify(header, payload, secret string, now time.Time, tolerance time.Duration) bool {
	ts, versions, ok := parseStripeSignature(header)
	if !ok {
		return false
	}
	if tolerance <= 0 {
		tolerance = DefaultWebhookTolerance
	}
	offset := now.Sub(time.Unix(ts, 0))
	if offset > tolerance || offset < -tolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + payload))
	want := mac.Sum(nil)
	for _, v := range versions {
		got, err := hex.DecodeString(v)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// parseStripeSignature 解析 Stripe-Signature 头：逗号分隔 key=value 段；
// t 必须存在且可解析为整数，且至少携带一个 v1，否则视为畸形拒绝。
func parseStripeSignature(header string) (ts int64, versions []string, ok bool) {
	foundT := false
	for _, part := range strings.Split(header, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, nil, false
			}
			ts, foundT = n, true
		case "v1":
			versions = append(versions, v)
		}
	}
	if !foundT || len(versions) == 0 {
		return 0, nil, false
	}
	return ts, versions, true
}

// truncateForLog 截断错误响应体用于错误信息（防超长刷屏）。
func truncateForLog(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
