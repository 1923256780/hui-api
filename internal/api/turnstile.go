// turnstile.go 人机校验（Turnstile siteverify）客户端（M3-wave1，docs/05）：
// 接口化设计便于测试 mock；真实实现 POST 表单到 siteverify 端点，
// 5s 超时（http.Client Timeout），网络/解析错误一律按「校验失败 + 错误返回」。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 运行轨配置键（options 白名单 turnstile.* 前缀）。
const (
	OptionKeyTurnstileEnabled   = "turnstile.enabled"
	OptionKeyTurnstileSiteKey   = "turnstile.site_key"
	OptionKeyTurnstileSecretKey = "turnstile.secret_key"
)

// siteverifyURL 是人机校验服务端校验端点（行业通用服务，配置方自担可达性）。
const siteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// turnstileVerifyTimeout 是 siteverify 请求超时（防注册链路被外部服务拖死）。
const turnstileVerifyTimeout = 5 * time.Second

// TurnstileVerifier 是人机校验服务端校验接口（测试可注入固定行为 mock）。
type TurnstileVerifier interface {
	// Verify 校验客户端提交的一次性 token；remoteIP 为发起方 IP（可选）。
	// 返回 (false, nil) 表示服务端判定不通过；(false, err) 表示校验过程失败
	//（网络/配置异常，调用方应按拒绝处理并记录错误）。
	Verify(ctx context.Context, secret, token, remoteIP string) (bool, error)
}

// siteverifyVerifier 是 TurnstileVerifier 的真实 HTTP 实现。
type siteverifyVerifier struct {
	client *http.Client
}

// newTurnstileVerifier 构造真实实现（5s 超时）。
func newTurnstileVerifier() *siteverifyVerifier {
	return &siteverifyVerifier{client: &http.Client{Timeout: turnstileVerifyTimeout}}
}

// siteverifyResponse 是 siteverify 响应体（error-codes 忽略细节，仅记日志用）。
type siteverifyResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// Verify 实现 TurnstileVerifier：POST application/x-www-form-urlencoded
// （secret/response/remoteip），2xx 且 success=true 判定通过。
func (v *siteverifyVerifier) Verify(ctx context.Context, secret, token, remoteIP string) (bool, error) {
	if strings.TrimSpace(secret) == "" {
		return false, fmt.Errorf("turnstile: secret 未配置")
	}
	form := url.Values{}
	form.Set("secret", secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, siteverifyURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return false, fmt.Errorf("turnstile: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("turnstile: 请求 siteverify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("turnstile: siteverify 状态码 %d", resp.StatusCode)
	}
	var out siteverifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("turnstile: 解析响应: %w", err)
	}
	return out.Success, nil
}
