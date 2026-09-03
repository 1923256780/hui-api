// order.go 在线充值订单端点（M3-wave3，docs/05 §5.10）：
//
//   - POST /api/user/topup/order（登录态）：创建充值订单——网关开关/配置
//     门控 → 金额区间校验 → 汇率换算（rate 快照与金额同记）→ 落 pending 单
//     → 返回收银台跳转 URL（epay submit / stripe session.url）。
//   - GET  /api/pay/epay/notify（公开）：异步通知验签 → 结算事务（幂等），
//     响应纯文本 success/fail（网关重试依据）。
//   - GET  /api/pay/epay/return（公开）：浏览器回跳 302 控制台（不做状态变更）。
//   - POST /api/pay/stripe/webhook（公开）：raw body 验签 → 结算事务（幂等）。
//   - GET  /api/user/topup/orders（登录态）：本人订单分页（所有权作用域），
//     支持 order_no 精确过滤（回跳查单/对账直取）。
//
// 结算事务（settleTopupOrder）沿用兑换码核销同一并发模型（SQLite 单写池 +
// 条件 UPDATE + RowsAffected 幂等）：网关/币种/金额逐位校验 → 条件迁移
// pending/expired→paid（RowsAffected=0 即重复通知，幂等跳过）→ 买家入账 →
// topup 日志 → aff 返利（inviter 存在且 rebate>0 时同事务累加，protocol=
// "aff"）。expired 一并纳入结算条件：验签与金额逐位校验通过即构成支付事实，
// 15min 超时关单后到达的真实支付通知（网关侧已扣款）仍复活入账，杜绝
// 「用户已扣款永不入账」的静默吞单。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/payment"
)

// 充值订单语义错误（映射 HTTP 状态，见 docs/05 错误码表）。
var (
	errOrderNotFound        = errors.New("order: not found")
	errOrderGatewayMismatch = errors.New("order: gateway mismatch")
	errOrderAmountMismatch  = errors.New("order: amount mismatch")
	errOrderUserMissing     = errors.New("order: buyer missing")
)

// 支付相关配置键（options 白名单 epay./stripe./topup./aff. 前缀）与常量。
const (
	OptionKeyEpayEnabled         = "epay.enabled"
	OptionKeyEpayGateway         = "epay.gateway"
	OptionKeyEpayPID             = "epay.pid"
	OptionKeyEpaySecretKey       = "epay.secret_key"
	OptionKeyEpayPayType         = "epay.pay_type"
	OptionKeyStripeEnabled       = "stripe.enabled"
	OptionKeyStripeSecretKey     = "stripe.secret_key"
	OptionKeyStripeWebhookSecret = "stripe.webhook_secret"
	OptionKeyTopupUSDCNYRate     = "topup.usd_cny_rate"
	OptionKeyTopupMinAmountCents = "topup.min_amount_cents"
	OptionKeyTopupMaxAmountCents = "topup.max_amount_cents"
	OptionKeyAffRebatePercent    = "aff.rebate_percent"

	// defaultUSDCNYRateCents 默认汇率定点（7.2 元/美元 → 720 分/美元）。
	defaultUSDCNYRateCents = 720

	// stripeHTTPTimeout 创建 Checkout Session 的出站超时。
	stripeHTTPTimeout = 30 * time.Second

	// stripeWebhookMaxBody webhook 请求体上限（1 MiB）。
	stripeWebhookMaxBody = 1 << 20

	// maxAmountHardCapCents 单笔充值金额硬上限（1e9 分 = 本币 1000 万元）：
	// 管理面 max 不限（0）或超配时的兜底封顶——防极限金额把 quota 换算
	// （amount×QuotaPerDollar）推入 int64 溢出、把 epay money 浮点格式化
	// 推入精度失控区。
	maxAmountHardCapCents = int64(1_000_000_000)
)

// requestBaseURL 由请求推导绝对基地址：scheme 信任反代注入的
// X-Forwarded-Proto（多值取第一段），无代理头回退 TLS 探测与 http 缺省——
// 与 oauthRedirectURI 同一信任顺序（反代部署义务注记 docs/11）。
func requestBaseURL(r *http.Request) string {
	scheme := ""
	if xp := r.Header.Get("X-Forwarded-Proto"); xp != "" {
		scheme = strings.TrimSpace(strings.Split(xp, ",")[0])
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

// newOrderNo 生成订单号：TP + unix 秒 + 12 字节随机 hex（order_no 唯一索引兜底）。
func newOrderNo() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成订单号: %w", err)
	}
	return "TP" + strconv.FormatInt(time.Now().Unix(), 10) + hex.EncodeToString(buf), nil
}

// topupQuotaFor 按汇率定点换算应入账额度：quota = round(amountCents ×
// QuotaPerDollar / rate)。rate 为「每 1 USD 折合本币分数 ×100」定点值
// （CNY 7.2 → 720；USD 本位 → 100），与契约公式
// amount_cents/(usd_cny_rate*100)*500000 等价；整数实现规避浮点误差。
func topupQuotaFor(amountCents, rate int64) int64 {
	return (amountCents*model.QuotaPerDollar + rate/2) / rate
}

// parseMoneyToMinor 把网关金额字符串（"10.00"/"10"/"10.5"）解析为最小货币
// 单位分：纯字符串逐位处理，规避二进制浮点误差。小数超出两位截断（EPay
// money 约定两位），负数/空串/非数字报错。
func parseMoneyToMinor(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("非法金额: %q", s)
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 2 {
		fracPart = fracPart[:2]
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	i, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("非法金额整数部分: %q", s)
	}
	f, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("非法金额小数部分: %q", s)
	}
	return i*100 + f, nil
}

// epayRateCents 读取 topup.usd_cny_rate（浮点字符串）并转定点（×100 四舍
// 五入）；未配置或非法回退默认 720（7.2 元/美元）。
func (h *Handler) epayRateCents() int64 {
	if v, ok := h.rt.Get(OptionKeyTopupUSDCNYRate); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			return int64(math.Round(f * 100))
		}
	}
	return defaultUSDCNYRateCents
}

// createTopupOrderRow 落一笔 pending 充值订单（TradeNo 为 stripe session id，
// epay 为空待回调回填）。
func (h *Handler) createTopupOrderRow(userID int64, orderNo, gateway, currency string, amountCents, quota, rate int64, tradeNo string) error {
	return h.st.Write.Create(&model.TopupOrder{
		OrderNo:     orderNo,
		UserID:      userID,
		Gateway:     gateway,
		AmountCents: amountCents,
		Currency:    currency,
		Quota:       quota,
		Rate:        rate,
		Status:      model.TopupOrderPending,
		TradeNo:     tradeNo,
		CreatedTime: time.Now().Unix(),
	}).Error
}

// CreateTopupOrder 创建充值订单（登录态）。流程：参数/金额区间校验 → 网关
// 开关与配置门控 → 汇率换算（rate 快照同记）→ 订单落库 → 返回跳转 URL。
// stripe 先建会话后落库（失败不产生孤儿单）；epay 无出站调用，先落库后拼 URL。
func (h *Handler) CreateTopupOrder(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		Gateway     string `json:"gateway"`
		AmountCents int64  `json:"amount_cents"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req.Gateway = strings.ToLower(strings.TrimSpace(req.Gateway))
	if req.Gateway != "epay" && req.Gateway != "stripe" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "gateway 仅支持 epay / stripe")
		return
	}
	// 金额区间：min 默认 100（分）；max 默认 0 = 不限（实际收敛到硬上限
	// 1e9 分 = 本币 1000 万元，见 maxAmountHardCapCents）。单位为订单币种
	// 的最小分数（epay 即 CNY 分，stripe 即 USD 分），契约注记 docs/05。
	minCents := h.rt.GetInt64(OptionKeyTopupMinAmountCents, 100)
	if minCents <= 0 {
		minCents = 1 // 配置兜底：至少 1 分
	}
	maxCents := h.rt.GetInt64(OptionKeyTopupMaxAmountCents, 0)
	if maxCents <= 0 || maxCents > maxAmountHardCapCents {
		maxCents = maxAmountHardCapCents // 不限/超配 → 硬上限兜底
	}
	if req.AmountCents < minCents || req.AmountCents > maxCents {
		writeErr(c, http.StatusBadRequest, "amount_out_of_range", "充值金额超出允许区间")
		return
	}
	// 汇率定点与应入账额度（快照与金额同记，审计可复核）。
	currency := "CNY"
	rate := int64(100) // USD 本位：每 1 USD = 100 分
	if req.Gateway == "epay" {
		rate = h.epayRateCents()
	}
	quota := topupQuotaFor(req.AmountCents, rate)

	orderNo, err := newOrderNo()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "topup_order_failed", "订单创建失败")
		return
	}
	reqBase := requestBaseURL(c.Request)
	var payURL, tradeNo string
	switch req.Gateway {
	case "epay":
		if !h.rt.GetBool(OptionKeyEpayEnabled, false) {
			writeErr(c, http.StatusForbidden, "gateway_disabled", "该支付方式未开放")
			return
		}
		gateway, _ := h.rt.Get(OptionKeyEpayGateway)
		pid, _ := h.rt.Get(OptionKeyEpayPID)
		secret, _ := h.rt.Get(OptionKeyEpaySecretKey)
		payType, _ := h.rt.Get(OptionKeyEpayPayType)
		gateway = strings.TrimSpace(gateway)
		if gateway == "" || strings.TrimSpace(pid) == "" || strings.TrimSpace(secret) == "" || strings.TrimSpace(payType) == "" {
			writeErr(c, http.StatusBadRequest, "gateway_not_configured", "支付网关配置不完整")
			return
		}
		payURL = payment.EPaySubmitURL(gateway, map[string]string{
			"pid":          pid,
			"type":         payType,
			"out_trade_no": orderNo,
			"notify_url":   reqBase + "/api/pay/epay/notify",
			"return_url":   reqBase + "/api/pay/epay/return",
			"name":         "余额充值",
			"money":        fmt.Sprintf("%.2f", float64(req.AmountCents)/100),
		}, secret)
	case "stripe":
		if !h.rt.GetBool(OptionKeyStripeEnabled, false) {
			writeErr(c, http.StatusForbidden, "gateway_disabled", "该支付方式未开放")
			return
		}
		secret, _ := h.rt.Get(OptionKeyStripeSecretKey)
		if strings.TrimSpace(secret) == "" {
			writeErr(c, http.StatusBadRequest, "gateway_not_configured", "支付网关配置不完整")
			return
		}
		currency = "USD"
		// 先创建会话（metadata[order_no] 回传关联），成功后落库——会话创建
		// 失败不产生孤儿 pending 单。
		session, err := payment.CreateCheckoutSession(c.Request.Context(), h.stripeHTTP,
			payment.CheckoutSessionParams{
				APIBase:     h.stripeAPIBase,
				SecretKey:   secret,
				OrderNo:     orderNo,
				AmountCents: req.AmountCents,
				Currency:    "usd",
				ProductName: "余额充值",
				SuccessURL:  reqBase + "/console/topup?order=" + url.QueryEscape(orderNo) + "&session_id={CHECKOUT_SESSION_ID}",
				CancelURL:   reqBase + "/console/topup?order=" + url.QueryEscape(orderNo),
			})
		if err != nil {
			writeErr(c, http.StatusBadGateway, "gateway_create_failed", "支付会话创建失败")
			return
		}
		tradeNo = session.ID
		payURL = session.URL
	}
	if err := h.createTopupOrderRow(u.ID, orderNo, req.Gateway, currency, req.AmountCents, quota, rate, tradeNo); err != nil {
		writeErr(c, http.StatusInternalServerError, "topup_order_failed", "订单创建失败")
		return
	}
	writeOK(c, gin.H{"order_no": orderNo, "pay_url": payURL, "quota": quota})
}

// settleTopupOrder 回调结算共享事务（epay notify 与 stripe webhook 复用）：
// 查单 → 网关/币种/金额逐位校验 → 条件 UPDATE pending/expired→paid
// （RowsAffected=0 即重复通知，幂等跳过；expired 纳入结算条件使超时关单后
// 到达的真实支付通知可复活入账）→ 买家入账 → topup 日志 → aff 返利
// （inviter 存在且 rebate>0：inviter.quota 与 aff_history_quota 同事务累加，
// protocol="aff"）。返回 settled=false 表示此前已结算（幂等跳过，调用方
// 仍回成功应答）。
func (h *Handler) settleTopupOrder(orderNo, gateway, tradeNo, currency string, moneyMinor int64, notifyRef string) (bool, error) {
	now := time.Now().Unix()
	pct := h.rt.GetInt64(OptionKeyAffRebatePercent, 0)
	settled := false
	err := h.st.Write.Transaction(func(tx *gorm.DB) error {
		var o model.TopupOrder
		if err := tx.Where("order_no = ?", orderNo).First(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errOrderNotFound
			}
			return errWrap("查询充值订单", err)
		}
		// 网关/币种/金额逐位校验：防跨网关混淆、币种混淆与降额攻击。
		if o.Gateway != gateway {
			return errOrderGatewayMismatch
		}
		if !strings.EqualFold(o.Currency, currency) {
			return errOrderAmountMismatch
		}
		if o.AmountCents != moneyMinor {
			return errOrderAmountMismatch
		}
		// 原子结算：条件 UPDATE pending/expired→paid。expired 一并纳入：
		// 验签与金额逐位校验通过即构成支付事实，超时关单（expired）后到达
		// 的真实支付通知仍复活入账（status 落 paid）；并发重复通知下另一
		// 方 RowsAffected=0，恰一次结算的幂等语义不变。
		res := tx.Model(&model.TopupOrder{}).
			Where("id = ? AND status IN ?", o.ID,
				[]int64{model.TopupOrderPending, model.TopupOrderExpired}).
			Updates(map[string]any{
				"status":    model.TopupOrderPaid,
				"trade_no":  tradeNo,
				"paid_time": now,
				"detail":    notifyRef,
			})
		if res.Error != nil {
			return errWrap("结算充值订单", res.Error)
		}
		if res.RowsAffected == 0 {
			return nil // 已结算过：幂等跳过（不重复入账、不重复返利）
		}
		// 买家入账。
		res = tx.Model(&model.User{}).Where("id = ?", o.UserID).
			Update("quota", gorm.Expr("quota + ?", o.Quota))
		if res.Error != nil {
			return errWrap("入账用户余额", res.Error)
		}
		if res.RowsAffected == 0 {
			return errOrderUserMissing
		}
		// 同事务 topup 日志（对账事实源）。
		if err := tx.Create(&model.Log{
			UserID:      o.UserID,
			Protocol:    "topup",
			ModelName:   "order_paid",
			Quota:       o.Quota,
			Detail:      topupDetail("order_paid", o.ID, o.Quota),
			CreatedTime: now,
		}).Error; err != nil {
			return errWrap("写充值日志", err)
		}
		// aff 返利：买家存在邀请人且返利比例 > 0 时同事务累加
		//（rebate = round(quota×pct/100)，整数式 (a×pct+50)/100）。
		if pct > 0 {
			var buyer model.User
			if err := tx.First(&buyer, o.UserID).Error; err != nil {
				return errWrap("查询买家", err)
			}
			if buyer.InviterID > 0 {
				rebate := (o.Quota*pct + 50) / 100
				if rebate > 0 {
					res := tx.Model(&model.User{}).Where("id = ?", buyer.InviterID).
						Updates(map[string]any{
							"quota":             gorm.Expr("quota + ?", rebate),
							"aff_history_quota": gorm.Expr("aff_history_quota + ?", rebate),
						})
					if res.Error != nil {
						return errWrap("入账邀请返利", res.Error)
					}
					// 邀请人已被删除（RowsAffected=0）：跳过返利不阻塞买家入账。
					if res.RowsAffected > 0 {
						if err := tx.Create(&model.Log{
							UserID:      buyer.InviterID,
							Protocol:    "aff",
							ModelName:   "topup_rebate",
							Quota:       rebate,
							Detail:      affRebateDetail(o.UserID, rebate),
							CreatedTime: now,
						}).Error; err != nil {
							return errWrap("写返利日志", err)
						}
					}
				}
			}
		}
		settled = true
		return nil
	})
	return settled, err
}

// affRebateDetail 构造 topup_rebate 日志 detail JSON（事件、买家、返利额度）。
func affRebateDetail(buyerID, rebate int64) string {
	b, err := json.Marshal(map[string]any{
		"event":  "topup_rebate",
		"ref_id": buyerID,
		"quota":  rebate,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// EpayNotify EPay 异步通知（公开）：取参（GET query 与 POST form 双形态
// 兼容，GET 优先）→ 验签 → trade_status 校验 → 金额逐位解析 → 结算事务。
// 响应纯文本 success/fail（网关重试依据）：任何失败（验签/缺单号/金额非法/
// 结算错误）回 fail 让网关重试；成功与幂等回 success。
func (h *Handler) EpayNotify(c *gin.Context) {
	// 取参合并：先 ParseForm 解析 POST 体（x-www-form-urlencoded，部分网关
	// 以 POST form 提交通知），再以 URL query 覆盖同键（GET 优先）。
	if err := c.Request.ParseForm(); err != nil {
		c.String(http.StatusOK, "fail")
		return
	}
	params := make(map[string]string, len(c.Request.PostForm)+len(c.Request.URL.Query()))
	for k, vs := range c.Request.PostForm {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}
	for k, vs := range c.Request.URL.Query() {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}
	secret, _ := h.rt.Get(OptionKeyEpaySecretKey)
	if secret == "" || !payment.EPayNotifyVerify(params, secret) {
		c.String(http.StatusOK, "fail")
		return
	}
	orderNo := params["out_trade_no"]
	if orderNo == "" {
		c.String(http.StatusOK, "fail")
		return
	}
	switch params["trade_status"] {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
	default:
		// 非成功态（交易关闭等）：确认已知悉防重发，但不入账不改状态。
		c.String(http.StatusOK, "success")
		return
	}
	minor, err := parseMoneyToMinor(params["money"])
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}
	if _, err := h.settleTopupOrder(orderNo, "epay", params["trade_no"], "CNY", minor, params["trade_no"]); err != nil {
		c.String(http.StatusOK, "fail")
		return
	}
	c.String(http.StatusOK, "success")
}

// EpayReturn EPay 浏览器回跳（公开）：302 到控制台充值页并携带订单号，前端
// 据此查单展示状态；真实入账以 notify 验签事务为准（return 不做状态变更，
// 不参与安全边界）。
func (h *Handler) EpayReturn(c *gin.Context) {
	orderNo := c.Query("out_trade_no")
	if orderNo == "" {
		orderNo = c.Query("order")
	}
	c.Redirect(http.StatusFound, "/console/topup?order="+url.QueryEscape(orderNo))
}

// StripeWebhook Stripe webhook（公开，raw body）：验签（Stripe-Signature 头
// t/v1，HMAC-SHA256 恒时比较 + ±5min 防重放）→ checkout.session.completed
// → metadata.order_no 查单 → 复用结算事务（幂等）→ 200。验签失败/载荷非法/
// 订单不符 4xx（Stripe 侧会重试）；其他事件类型确认忽略。
func (h *Handler) StripeWebhook(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, stripeWebhookMaxBody))
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	secret, _ := h.rt.Get(OptionKeyStripeWebhookSecret)
	if secret == "" || !payment.WebhookVerify(c.GetHeader("Stripe-Signature"), string(raw), secret, time.Now(), 0) {
		writeErr(c, http.StatusBadRequest, "invalid_signature", "回调签名校验失败")
		return
	}
	var event struct {
		Type string `json:"type"`
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", "回调载荷解析失败")
		return
	}
	if event.Type != "checkout.session.completed" {
		writeOK(c, gin.H{"ignored": event.Type})
		return
	}
	var obj struct {
		ID          string `json:"id"`
		AmountTotal int64  `json:"amount_total"`
		Currency    string `json:"currency"`
		Metadata    struct {
			OrderNo string `json:"order_no"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(event.Data.Object, &obj); err != nil || obj.Metadata.OrderNo == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "回调缺少订单标识")
		return
	}
	if _, err := h.settleTopupOrder(obj.Metadata.OrderNo, "stripe", obj.ID, obj.Currency, obj.AmountTotal, obj.ID); err != nil {
		switch {
		case errors.Is(err, errOrderNotFound):
			writeErr(c, http.StatusNotFound, "not_found", "订单不存在")
		case errors.Is(err, errOrderGatewayMismatch), errors.Is(err, errOrderAmountMismatch):
			writeErr(c, http.StatusBadRequest, "order_mismatch", "订单信息与回调不符")
		case errors.Is(err, errOrderUserMissing):
			writeErr(c, http.StatusInternalServerError, "settle_failed", "订单结算失败")
		default:
			writeErr(c, http.StatusInternalServerError, "settle_failed", "订单结算失败")
		}
		return
	}
	writeOK(c, nil)
}

// ListMyTopupOrders 当前登录用户本人的充值订单分页（所有权作用域，id desc），
// 可选 order_no 精确过滤（回跳查单/对账直取，仍限本人单）。
func (h *Handler) ListMyTopupOrders(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	page, pageSize := pagination(c)
	q := h.st.Read.Model(&model.TopupOrder{}).Where("user_id = ?", u.ID)
	if no := strings.TrimSpace(c.Query("order_no")); no != "" {
		q = q.Where("order_no = ?", no)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询订单失败")
		return
	}
	var rows []model.TopupOrder
	if err := q.Order("id desc").Offset((page - 1) * pageSize).
		Limit(pageSize).Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询订单失败")
		return
	}
	writeOK(c, gin.H{"items": rows, "total": total, "page": page, "page_size": pageSize})
}
