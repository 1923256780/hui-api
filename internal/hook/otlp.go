// otlp.go OTLP/HTTP 指标导出 Hook（M2-wave3，docs/05 hooks 契约）：
//
//   - 事件路径只做内存累加（互斥锁内微秒级），独立 goroutine 按固定间隔导出，
//     Stop 时冲刷一次——任何失败降级为丢弃计数，永不阻塞转发主链路；
//   - 传输：OTLP/HTTP JSON（application/json，POST <endpoint>/v1/metrics），
//     累计 temporality（=2）：序列 startTime 固定，每次导出发全量累计值；
//   - 指标集（属性 model/outcome/kind 标识序列维度）：
//     hui.request.duration_ms  histogram，单位 ms
//     hui.request.tokens       histogram，kind=prompt|completion
//     hui.request.status       monotonic sum（成功/失败计数）
//   - endpoint 经 EndpointFn 动态读取（hooks.otlp.endpoint 热更即时生效），
//     未配置时静默跳过；导出超时 3s，失败丢弃并计数。
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// options 键与导出参数。
const (
	// OptionKeyOTLPEndpoint 是 OTLP 导出端点键（hooks.otlp.endpoint，热更）。
	OptionKeyOTLPEndpoint = "hooks.otlp.endpoint"

	otlpExportInterval = 2 * time.Second
	otlpHTTPTimeout    = 3 * time.Second
	otlpMetricsPath    = "/v1/metrics"

	// 指标名（桶边界覆盖 10ms~10s 量级，末桶为 Inf）。
	durationMetric = "hui.request.duration_ms"
	tokensMetric   = "hui.request.tokens"
	statusMetric   = "hui.request.status"
)

// histBounds 是耗时/用量直方图的显式上界（数据点数 = len+1，末桶 Inf）。
var histBounds = []float64{10, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// EndpointFn 动态读取配置（返回空串表示未启用导出）。
type EndpointFn func() string

// otlpAttr 是序列属性（导出时按键排序保证稳定编码）。
type otlpAttr struct{ Key, Value string }

// otlpHistPoint 是一个 histogram 序列的累计状态。
type otlpHistPoint struct {
	name    string
	attrs   []otlpAttr
	count   uint64
	sum     float64
	buckets []uint64 // len(histBounds)+1，末桶为 Inf
}

// otlpSumPoint 是一个 monotonic counter 序列的累计状态。
type otlpSumPoint struct {
	attrs []otlpAttr
	value int64
}

// OTLP 是 OTLP/HTTP 指标导出 Hook。并发安全（事件累加与导出快照共用互斥锁）。
type OTLP struct {
	endpoint EndpointFn
	client   *http.Client

	mu       sync.Mutex
	hist     map[string]*otlpHistPoint
	counters map[string]*otlpSumPoint
	started  time.Time

	dropped atomic.Int64 // 导出失败计数（网络/超时/非 2xx/编码失败）
	stop    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
}

// NewOTLP 构造并启动导出循环。
func NewOTLP(endpoint EndpointFn) *OTLP {
	o := &OTLP{
		endpoint: endpoint,
		client:   &http.Client{Timeout: otlpHTTPTimeout},
		hist:     make(map[string]*otlpHistPoint),
		counters: make(map[string]*otlpSumPoint),
		started:  time.Now(),
		stop:     make(chan struct{}),
	}
	o.wg.Add(1)
	go o.loop()
	return o
}

// Name 实现 Hook 接口。
func (o *OTLP) Name() string { return "otlp" }

// OnSuccess 实现 Hook 接口（成功观测：仅内存累加）。
func (o *OTLP) OnSuccess(_ context.Context, ev Event) error {
	o.record(ev, "success")
	return nil
}

// OnFailure 实现 Hook 接口（失败观测：仅内存累加）。
func (o *OTLP) OnFailure(_ context.Context, ev Event) error {
	o.record(ev, "failed")
	return nil
}

// Dropped 返回累计导出失败次数（观测用）。
func (o *OTLP) Dropped() int64 { return o.dropped.Load() }

// Stop 停止导出循环并冲刷最后一次快照。幂等。
func (o *OTLP) Stop(timeout time.Duration) {
	o.once.Do(func() { close(o.stop) })
	done := make(chan struct{})
	go func() { o.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// record 累加一次请求观测：耗时直方图、tokens 直方图与状态计数。
func (o *OTLP) record(ev Event, outcome string) {
	base := []otlpAttr{{Key: "model", Value: ev.Model}, {Key: "outcome", Value: outcome}}
	if d, ok := dataFloat(ev.Data, "duration_ms"); ok {
		o.observeHistogram(durationMetric, base, d)
	}
	if p, ok := dataFloat(ev.Data, "prompt_tokens"); ok {
		o.observeHistogram(tokensMetric, append(base, otlpAttr{Key: "kind", Value: "prompt"}), p)
	}
	if c, ok := dataFloat(ev.Data, "completion_tokens"); ok {
		o.observeHistogram(tokensMetric, append(base, otlpAttr{Key: "kind", Value: "completion"}), c)
	}
	o.incrementCounter(base, 1)
}

// observeHistogram 累加一次直方图观测。
func (o *OTLP) observeHistogram(name string, attrs []otlpAttr, v float64) {
	key := name + "\x00" + attrsKey(attrs)
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.hist[key]
	if p == nil {
		p = &otlpHistPoint{name: name, attrs: attrs, buckets: make([]uint64, len(histBounds)+1)}
		o.hist[key] = p
	}
	p.count++
	p.sum += v
	for i, b := range histBounds {
		if v <= b {
			p.buckets[i]++
			return
		}
	}
	p.buckets[len(histBounds)]++
}

// incrementCounter 累加状态计数序列。
func (o *OTLP) incrementCounter(attrs []otlpAttr, delta int64) {
	key := statusMetric + "\x00" + attrsKey(attrs)
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.counters[key]
	if p == nil {
		p = &otlpSumPoint{attrs: attrs}
		o.counters[key] = p
	}
	p.value += delta
}

// loop 是导出循环：定时快照导出，停止时冲刷。
func (o *OTLP) loop() {
	defer o.wg.Done()
	ticker := time.NewTicker(otlpExportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			o.export()
		case <-o.stop:
			o.export()
			return
		}
	}
}

// export 快照当前累计序列并 POST 到 OTLP/HTTP 端点。任何失败只计数不外泄。
func (o *OTLP) export() {
	endpoint := strings.TrimSpace(o.endpoint())
	if endpoint == "" {
		return
	}
	payload := o.snapshot()
	if payload == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		o.dropped.Add(1)
		return
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint, "/")+otlpMetricsPath, bytes.NewReader(body))
	if err != nil {
		o.dropped.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		o.dropped.Add(1)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		o.dropped.Add(1)
	}
}

// snapshot 组装累计序列的 OTLP/HTTP JSON 载荷（无数据返回 nil，跳过空导出）。
func (o *OTLP) snapshot() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.hist) == 0 && len(o.counters) == 0 {
		return nil
	}
	startNano := strconv.FormatInt(o.started.UnixNano(), 10)
	nowNano := strconv.FormatInt(time.Now().UnixNano(), 10)

	// histogram 数据点按指标名分组成 metrics（duration/tokens 各一条）。
	byName := map[string][]map[string]any{}
	for _, p := range o.hist {
		byName[p.name] = append(byName[p.name], p.jsonPoint(startNano, nowNano))
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	metrics := make([]map[string]any, 0, len(byName)+1)
	for _, name := range names {
		metric := map[string]any{"name": name, "histogram": map[string]any{
			"aggregationTemporality": 2,
			"dataPoints":             byName[name],
		}}
		if name == durationMetric {
			metric["unit"] = "ms"
		}
		metrics = append(metrics, metric)
	}
	if len(o.counters) > 0 {
		points := make([]map[string]any, 0, len(o.counters))
		for _, p := range o.counters {
			points = append(points, map[string]any{
				"attributes":        jsonAttrs(p.attrs),
				"startTimeUnixNano": startNano,
				"timeUnixNano":      nowNano,
				"asInt":             strconv.FormatInt(p.value, 10),
			})
		}
		metrics = append(metrics, map[string]any{"name": statusMetric, "sum": map[string]any{
			"aggregationTemporality": 2,
			"isMonotonic":            true,
			"dataPoints":             points,
		}})
	}
	return map[string]any{
		"resourceMetrics": []map[string]any{{
			"resource": map[string]any{
				"attributes": []map[string]any{
					{"key": "service.name", "value": map[string]any{"stringValue": "hui-api"}},
				},
			},
			"scopeMetrics": []map[string]any{{
				"scope":   map[string]any{"name": "github.com/1923256780/hui-api"},
				"metrics": metrics,
			}},
		}},
	}
}

// jsonPoint 序列化一个 histogram 数据点（bucketCounts 含 Inf 桶）。
func (p *otlpHistPoint) jsonPoint(startNano, nowNano string) map[string]any {
	counts := make([]string, len(p.buckets))
	for i, c := range p.buckets {
		counts[i] = strconv.FormatUint(c, 10)
	}
	return map[string]any{
		"attributes":        jsonAttrs(p.attrs),
		"startTimeUnixNano": startNano,
		"timeUnixNano":      nowNano,
		"count":             strconv.FormatUint(p.count, 10),
		"sum":               p.sum,
		"bucketCounts":      counts,
		"explicitBounds":    histBounds,
	}
}

// attrsKey 生成序列键：属性按键排序后拼接（同键必同序，保证聚合稳定）。
func attrsKey(attrs []otlpAttr) string {
	sorted := append([]otlpAttr(nil), attrs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	parts := make([]string, len(sorted))
	for i, a := range sorted {
		parts[i] = a.Key + "=" + a.Value
	}
	return strings.Join(parts, "\x01")
}

// jsonAttrs 编码属性列表（按键排序）。
func jsonAttrs(attrs []otlpAttr) []map[string]any {
	sorted := append([]otlpAttr(nil), attrs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	out := make([]map[string]any, len(sorted))
	for i, a := range sorted {
		out[i] = map[string]any{"key": a.Key, "value": map[string]any{"stringValue": a.Value}}
	}
	return out
}

// dataFloat 从事件 Data 提取数值（兼容 int/int64/float64 编码）。
func dataFloat(data map[string]any, key string) (float64, bool) {
	if data == nil {
		return 0, false
	}
	switch v := data[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// 编译期保证满足 Hook 接口。
var _ Hook = (*OTLP)(nil)
