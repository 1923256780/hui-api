// Package payment 实现 M3-wave3 在线充值的支付网关适配层（docs/05 §5.10）：
//
//   - EPay（易支付协议，本文件）：MD5 签名、收银台跳转 URL 构造、异步通知
//     验签——全部为纯函数，无全局状态、无网络交互。
//   - Stripe（stripe.go）：Checkout Session 创建与 Webhook 签名校验。
//
// 金额/订单业务语义（金额逐位校验、幂等入账、返利）在 internal/api/order.go，
// 本包只负责「签名与协议」这一层，便于对固定向量做单元测试。
package payment

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// epayUnsignedKeys 不参与 MD5 签名的参数：sign 本身与签名算法声明。
var epayUnsignedKeys = map[string]bool{"sign": true, "sign_type": true}

// epaySignBase 构造待签名串：过滤空值与 sign/sign_type，参数名字典序，
// 逐对拼接 k=v&（每对带尾 &），末尾直接接商户密钥。独立成函数便于测试
// 断言签名串的精确格式。
func epaySignBase(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" || epayUnsignedKeys[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
		b.WriteByte('&')
	}
	b.WriteString(key)
	return b.String()
}

// EPaySign 计算易支付 MD5 签名：MD5(签名串) 的 hex 小写。
// MD5 是易支付协议规定的固定签名算法（协议兼容性约束，非本仓库安全选型）——
// 签名密钥仅服务端持有，且验签走恒时比较；仓库内其余安全敏感场景一律使用
// SHA-256（见 stripe.go 的 HMAC-SHA256）。本函数不用于任何非易支付协议场景。
func EPaySign(params map[string]string, key string) string {
	sum := md5.Sum([]byte(epaySignBase(params, key)))
	return hex.EncodeToString(sum[:])
}

// EPaySubmitURL 构造收银台跳转地址：网关根地址（容忍尾斜杠）+ /submit.php?
// + 业务参数（URL 编码，字典序）+ sign_type=MD5 + sign。调用方把用户以
// GET/POST 重定向到该地址。
func EPaySubmitURL(gateway string, params map[string]string, key string) string {
	vals := url.Values{}
	for k, v := range params {
		if v == "" || epayUnsignedKeys[k] {
			continue
		}
		vals.Set(k, v)
	}
	vals.Set("sign_type", "MD5")
	vals.Set("sign", EPaySign(params, key))
	return strings.TrimRight(gateway, "/") + "/submit.php?" + vals.Encode()
}

// EPayNotifyVerify 异步通知验签：通知参数含 sign（hex）与 sign_type=MD5，
// 按同一签名串规则重算并与通知方 sign 恒时比较（subtle.ConstantTimeCompare，
// 防时序侧信道）。sign 缺失直接拒绝；比较前统一小写以兼容大写 hex 的通知方。
func EPayNotifyVerify(params map[string]string, key string) bool {
	sign := params["sign"]
	if sign == "" {
		return false
	}
	want := EPaySign(params, key)
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(sign)), []byte(want)) == 1
}
