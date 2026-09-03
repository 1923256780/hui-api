// webhook.go Webhook 通知 Hook（M2-wave3，docs/05 hooks 契约）：
//
//   - POST 事件 JSON 到配置 URL（hooks.webhook.url 经 URLFn 动态读取，热更即时生效；
//     URL 由 root 管理员配置，信任边界与目标可达性由配置方负责）；
//   - 超时 3s（client timeout，另受 dispatcher 单事件投递超时约束）；失败
//     （网络/超时/非 2xx/编码失败）丢弃并计数，绝不重试、绝不阻塞主链路；
//   - 载荷字段：type / request_id / token_id / channel_id / model / error /
//     timestamp / idempotency_key / data；成功与失败事件同一格式（error 非空
//     即失败事件，由 Dispatcher 按 Event.Err 路由到 OnFailure）。
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// options 键与投递参数。
const (
	// OptionKeyWebhookURL 是 webhook 目标 URL 键（hooks.webhook.url，热更）。
	OptionKeyWebhookURL = "hooks.webhook.url"

	webhookHTTPTimeout = 3 * time.Second
	webhookRespLimit   = 4 << 10 // 响应体读取上限（丢弃读，防慢响应拖住连接）
)

// URLFn 动态读取 webhook 目标 URL（返回空串表示未启用投递）。
type URLFn func() string

// Webhook 是 HTTP 回调 Hook。并发安全。
type Webhook struct {
	url     URLFn
	client  *http.Client
	dropped atomic.Int64
}

// NewWebhook 构造 webhook Hook。
func NewWebhook(url URLFn) *Webhook {
	return &Webhook{
		url:    url,
		client: &http.Client{Timeout: webhookHTTPTimeout},
	}
}

// Name 实现 Hook 接口。
func (w *Webhook) Name() string { return "webhook" }

// OnSuccess 实现 Hook 接口。
func (w *Webhook) OnSuccess(ctx context.Context, ev Event) error { return w.deliver(ctx, ev) }

// OnFailure 实现 Hook 接口。
func (w *Webhook) OnFailure(ctx context.Context, ev Event) error { return w.deliver(ctx, ev) }

// Dropped 返回累计丢弃数（网络失败/超时/非 2xx/编码失败）。
func (w *Webhook) Dropped() int64 { return w.dropped.Load() }

// deliver 序列化事件并 POST 到配置 URL。未配置 URL 时静默跳过；
// 失败丢弃计数并返回 nil（投递失败非 hook 契约错误，避免 dispatcher 重复记日志）。
func (w *Webhook) deliver(ctx context.Context, ev Event) error {
	endpoint := strings.TrimSpace(w.url())
	if endpoint == "" {
		return nil
	}
	payload := map[string]any{
		"type":            ev.Type,
		"request_id":      ev.RequestID,
		"token_id":        ev.TokenID,
		"channel_id":      ev.ChannelID,
		"model":           ev.Model,
		"error":           ev.Err,
		"timestamp":       ev.Timestamp.UTC().Format(time.RFC3339Nano),
		"idempotency_key": ev.IdempotencyKey,
		"data":            ev.Data,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		w.dropped.Add(1)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		w.dropped.Add(1)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		w.dropped.Add(1)
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookRespLimit))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		w.dropped.Add(1)
	}
	return nil
}

// 编译期保证满足 Hook 接口。
var _ Hook = (*Webhook)(nil)
