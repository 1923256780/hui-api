// webhook_test.go Webhook 投递测试（M2-wave3）：载荷字段完整、未配置静默跳过、
// 不可达/非 2xx 丢弃计数。
package hook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestWebhookDeliver 正常投递：POST JSON、字段完整、幂等键透传。
func TestWebhookDeliver(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type 应为 application/json，实际 %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		payloads = append(payloads, m)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	w := NewWebhook(func() string { return srv.URL })
	ev := Event{
		Type: "request.completed", RequestID: "req-1", TokenID: 7, ChannelID: 3,
		Model: "m1", Timestamp: time.Now(), IdempotencyKey: "req-1:completed",
		Data: map[string]any{"quota": int64(42)},
	}
	if err := w.OnSuccess(context.Background(), ev); err != nil {
		t.Fatalf("投递不应报错: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 1 {
		t.Fatalf("应投递 1 次，实际 %d", len(payloads))
	}
	p := payloads[0]
	if p["type"] != "request.completed" || p["request_id"] != "req-1" ||
		p["idempotency_key"] != "req-1:completed" || p["model"] != "m1" {
		t.Fatalf("载荷字段不符: %v", p)
	}
	if p["error"] != "" {
		t.Fatalf("成功事件 error 应为空: %v", p["error"])
	}
	data, ok := p["data"].(map[string]any)
	if !ok || data["quota"] != float64(42) {
		t.Fatalf("data 应透传: %v", p["data"])
	}
	if _, err := time.Parse(time.RFC3339Nano, p["timestamp"].(string)); err != nil {
		t.Fatalf("timestamp 应为 RFC3339Nano: %v", err)
	}
}

// TestWebhookDegrade 降级路径：不可达/非 2xx 计数、未配置静默。
func TestWebhookDegrade(t *testing.T) {
	// 不可达端点：连接拒绝 → 丢弃计数。
	w := NewWebhook(func() string { return "http://127.0.0.1:1/hook" })
	_ = w.OnSuccess(context.Background(), Event{Type: "x", Timestamp: time.Now()})
	if w.Dropped() != 1 {
		t.Fatalf("不可达应计 1 次丢弃，实际 %d", w.Dropped())
	}

	// 非 2xx：丢弃计数。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	w2 := NewWebhook(func() string { return srv.URL })
	_ = w2.OnSuccess(context.Background(), Event{Type: "x", Timestamp: time.Now()})
	if w2.Dropped() != 1 {
		t.Fatalf("非 2xx 应计 1 次丢弃，实际 %d", w2.Dropped())
	}

	// 未配置 URL：静默跳过不计。
	w3 := NewWebhook(func() string { return "" })
	_ = w3.OnSuccess(context.Background(), Event{Type: "x", Timestamp: time.Now()})
	if w3.Dropped() != 0 {
		t.Fatalf("未配置不应计数，实际 %d", w3.Dropped())
	}
}
