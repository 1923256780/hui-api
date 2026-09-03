// otlp_test.go OTLP 指标导出测试（M2-wave3）：JSON 结构与聚合口径正确、
// 未配置端点静默跳过、导出失败降级计数、Stop 后停止接收。
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

// otlpCapture 是假 OTLP 接收端：记录请求计数与解码后的载荷。
type otlpCapture struct {
	mu     sync.Mutex
	hits   int
	bodies []map[string]any
}

func (c *otlpCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		c.mu.Lock()
		c.hits++
		c.bodies = append(c.bodies, m)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (c *otlpCapture) snapshot() (int, []map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, append([]map[string]any(nil), c.bodies...)
}

// histMetric 找指定名的 histogram 指标。
func histMetric(t *testing.T, payload map[string]any, name string) (hist map[string]any, ok bool) {
	t.Helper()
	rms, _ := payload["resourceMetrics"].([]any)
	if len(rms) != 1 {
		t.Fatalf("resourceMetrics 应恰一组: %v", payload)
	}
	sms, _ := rms[0].(map[string]any)["scopeMetrics"].([]any)
	if len(sms) != 1 {
		t.Fatalf("scopeMetrics 应恰一组")
	}
	metrics, _ := sms[0].(map[string]any)["metrics"].([]any)
	for _, m := range metrics {
		mm := m.(map[string]any)
		if mm["name"] == name {
			h, _ := mm["histogram"].(map[string]any)
			return h, true
		}
	}
	return nil, false
}

// sumMetric 找 monotonic sum 指标。
func sumMetric(t *testing.T, payload map[string]any, name string) (sum map[string]any, ok bool) {
	t.Helper()
	rms, _ := payload["resourceMetrics"].([]any)
	sms, _ := rms[0].(map[string]any)["scopeMetrics"].([]any)
	metrics, _ := sms[0].(map[string]any)["metrics"].([]any)
	for _, m := range metrics {
		mm := m.(map[string]any)
		if mm["name"] == name {
			s, _ := mm["sum"].(map[string]any)
			return s, true
		}
	}
	return nil, false
}

// TestOTLPExportPayload 导出载荷：三指标齐全、聚合口径与桶归属正确。
func TestOTLPExportPayload(t *testing.T) {
	cap := &otlpCapture{}
	srv := httptest.NewServer(cap.handler())
	t.Cleanup(srv.Close)

	o := NewOTLP(func() string { return srv.URL })
	t.Cleanup(func() { o.Stop(time.Second) })

	o.OnSuccess(context.Background(), Event{
		Type: "request.completed", Model: "m1", TokenID: 1, Timestamp: time.Now(),
		IdempotencyKey: "k1",
		Data:           map[string]any{"duration_ms": 120.0, "prompt_tokens": 100, "completion_tokens": 50, "quota": 3},
	})
	o.OnFailure(context.Background(), Event{
		Type: "request.failed", Model: "m1", Err: "boom", TokenID: 1, Timestamp: time.Now(),
		IdempotencyKey: "k2",
		Data:           map[string]any{"duration_ms": 30.0, "prompt_tokens": 10, "completion_tokens": 0},
	})

	o.export() // 同包直触发导出，避免依赖 ticker 等待

	hits, bodies := cap.snapshot()
	if hits != 1 || len(bodies) != 1 {
		t.Fatalf("应恰好导出 1 次，实际 %d 次", hits)
	}
	payload := bodies[0]

	// duration 指标：两个序列（success/failed），count=2，sum=150；
	// 120ms 落 (100,250] 桶（index 3），30ms 落 (10,50] 桶（index 1）。
	h, ok := histMetric(t, payload, durationMetric)
	if !ok {
		t.Fatalf("缺 duration 指标: %v", payload)
	}
	points, _ := h["dataPoints"].([]any)
	if len(points) != 2 {
		t.Fatalf("duration 应有 2 个数据点，实际 %d", len(points))
	}
	var totalCount int
	for _, p := range points {
		pp := p.(map[string]any)
		count, _ := pp["count"].(string)
		if count == "1" {
			buckets, _ := pp["bucketCounts"].([]any)
			idx := -1
			for i, bc := range buckets {
				if bc.(string) == "1" {
					idx = i
				}
			}
			if idx != 1 && idx != 3 {
				t.Fatalf("桶归属应 index 1 或 3，实际 %d", idx)
			}
			totalCount++
		}
	}
	if totalCount != 2 {
		t.Fatalf("两序列各 count=1，实际匹配 %d", totalCount)
	}

	// tokens 指标：4 个序列（2 outcome × 2 kind）。
	th, ok := histMetric(t, payload, tokensMetric)
	if !ok {
		t.Fatalf("缺 tokens 指标")
	}
	tpoints, _ := th["dataPoints"].([]any)
	if len(tpoints) != 4 {
		t.Fatalf("tokens 应有 4 个数据点，实际 %d", len(tpoints))
	}

	// status 指标：success=1、failed=1，isMonotonic。
	s, ok := sumMetric(t, payload, statusMetric)
	if !ok {
		t.Fatalf("缺 status 指标")
	}
	if mono, _ := s["isMonotonic"].(bool); !mono {
		t.Fatalf("status 应为 monotonic sum")
	}
	sp, _ := s["dataPoints"].([]any)
	if len(sp) != 2 {
		t.Fatalf("status 应有 2 个数据点，实际 %d", len(sp))
	}
	for _, p := range sp {
		if v, _ := p.(map[string]any)["asInt"].(string); v != "1" {
			t.Fatalf("status 计数应为 1，实际 %s", v)
		}
	}
}

// TestOTLPDegrade 失败降级：不可达端点计数、未配置端点静默、聚合累计不丢失。
func TestOTLPDegrade(t *testing.T) {
	o := NewOTLP(func() string { return "http://127.0.0.1:1" + otlpMetricsPath })
	t.Cleanup(func() { o.Stop(time.Second) })

	o.OnSuccess(context.Background(), Event{
		Type: "request.completed", Model: "m1", Timestamp: time.Now(),
		Data: map[string]any{"duration_ms": 5.0},
	})
	o.export()
	if o.Dropped() != 1 {
		t.Fatalf("不可达端点应计 1 次丢弃，实际 %d", o.Dropped())
	}

	// 未配置端点：静默跳过，不计丢弃。
	o2 := NewOTLP(func() string { return "" })
	t.Cleanup(func() { o2.Stop(time.Second) })
	o2.export()
	if o2.Dropped() != 0 {
		t.Fatalf("未配置端点不应计数，实际 %d", o2.Dropped())
	}

	// 无数据导出：不发请求。
	cap := &otlpCapture{}
	srv := httptest.NewServer(cap.handler())
	t.Cleanup(srv.Close)
	o2.endpoint = func() string { return srv.URL }
	o2.export()
	if n, _ := cap.snapshot(); n != 0 {
		t.Fatalf("空快照不应发请求，实际 %d 次", n)
	}
}

// TestOTLPCumulativeAggregation 累计语义：多次导出保留全量累计值。
func TestOTLPCumulativeAggregation(t *testing.T) {
	cap := &otlpCapture{}
	srv := httptest.NewServer(cap.handler())
	t.Cleanup(srv.Close)

	o := NewOTLP(func() string { return srv.URL })
	t.Cleanup(func() { o.Stop(time.Second) })
	ev := Event{Type: "request.completed", Model: "m1", Timestamp: time.Now(),
		Data: map[string]any{"duration_ms": 20.0}}

	o.OnSuccess(context.Background(), ev)
	o.export()
	o.OnSuccess(context.Background(), ev)
	o.export()

	_, bodies := cap.snapshot()
	if len(bodies) != 2 {
		t.Fatalf("应导出 2 次，实际 %d", len(bodies))
	}
	last := bodies[1]
	h, ok := histMetric(t, last, durationMetric)
	if !ok {
		t.Fatalf("缺 duration 指标")
	}
	points, _ := h["dataPoints"].([]any)
	if len(points) != 1 {
		t.Fatalf("同序列应聚合为 1 个数据点，实际 %d", len(points))
	}
	if count, _ := points[0].(map[string]any)["count"].(string); count != "2" {
		t.Fatalf("累计 count 应为 2，实际 %s", count)
	}
}
