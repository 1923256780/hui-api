package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/relay/anthropic"
	"github.com/1923256780/hui-api/internal/relay/openai"
	"github.com/1923256780/hui-api/internal/store"
)

// ---- 测试基建 ----

// newTestGateway 构造临时库 + 运行轨 + 网关，并写入一枚启用令牌，返回明文。
// options 为预写入的运行轨键值（构造 Runtime 前落库，随首次加载生效）。
func newTestGateway(t *testing.T, options map[string]string) (*Gateway, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	for k, v := range options {
		if err := st.Write.Create(&model.Option{Key: k, Value: v}).Error; err != nil {
			t.Fatalf("写入 option %s 失败: %v", k, err)
		}
	}
	rt, err := config.NewRuntime(st)
	if err != nil {
		t.Fatalf("构造运行轨失败: %v", err)
	}
	g := New(st, rt)
	plain := seedToken(t, st, nil)
	return g, st, plain
}

// ginCtx 构造 gin 测试上下文与响应记录器（Recorder 支持 Flush，可验证流式）。
func ginCtx(t *testing.T, method, target string, body []byte, header http.Header) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req := httptest.NewRequest(method, target, r)
	if header != nil {
		req.Header = header
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

// upstreamHit 记录一次上游请求的观测数据（编排层断言用）。
type upstreamHit struct {
	Path string
	Auth string
	Body []byte
}

// fakeUpstream 构造假上游：返回服务地址与观测记录（同步时序下读写天然串行）。
func fakeUpstream(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body []byte)) (*httptest.Server, *[]upstreamHit) {
	t.Helper()
	hits := &[]upstreamHit{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*hits = append(*hits, upstreamHit{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
		handle(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// postChat 构造一次 OpenAI 协议的 Serve 调用，返回响应记录器。
func postChat(t *testing.T, g *Gateway, plain string, body []byte) *httptest.ResponseRecorder {
	c, w := ginCtx(t, http.MethodPost, "/v1/chat/completions", body,
		http.Header{"Authorization": []string{"Bearer " + plain}})
	g.Serve(c, openai.New())
	return w
}

// ---- 编排级测试 ----

// TestGatewayHappyPath OpenAI 非流式：转发成功、上游路径/渠道密钥替换正确。
func TestGatewayHappyPath(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":34,"prompt_tokens_details":{"cached_tokens":7}}}`))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "sk-channel-secret", Models: "m1"})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[{"role":"user","content":"hello"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"content":"hi"`) {
		t.Fatalf("响应体应透传上游内容: %s", w.Body.String())
	}
	if len(*hits) != 1 {
		t.Fatalf("上游应收到 1 次请求，实际 %d", len(*hits))
	}
	h := (*hits)[0]
	if h.Path != "/v1/chat/completions" {
		t.Fatalf("上游路径不符: %s", h.Path)
	}
	if h.Auth != "Bearer sk-channel-secret" {
		t.Fatalf("上游鉴权应为渠道密钥: %q", h.Auth)
	}
}

// TestGatewayStreamRelay 流式：SSE 原文逐事件转发、发生 Flush、不改写内容。
func TestGatewayStreamRelay(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	sse := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte(sse))
	})
	seedChannel(t, st, model.Channel{Name: "up", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "sk-x", Models: "m1"})

	w := postChat(t, g, plain, []byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != sse {
		t.Fatalf("流式响应应与上游原文逐字节一致:\n got=%q\nwant=%q", got, sse)
	}
	if !w.Flushed {
		t.Fatal("应发生 Flush（逐事件推送）")
	}
}

// TestGatewayNoChannel 模型无可用渠道 → 503 语义错误。
func TestGatewayNoChannel(t *testing.T) {
	g, _, plain := newTestGateway(t, nil)
	w := postChat(t, g, plain, []byte(`{"model":"absent","messages":[]}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("应 503，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no_available_channel") {
		t.Fatalf("错误码应为 no_available_channel: %s", w.Body.String())
	}
}

// TestGatewayUnauthorized 缺 key 与错 key 均 401。
func TestGatewayUnauthorized(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)

	c, w := ginCtx(t, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"m1"}`), http.Header{})
	g.Serve(c, openai.New())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺 key 应 401，实际 %d body=%s", w.Code, w.Body.String())
	}

	w = postChat(t, g, "sk-wrong", []byte(`{"model":"m1"}`))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错 key 应 401，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestGatewayMissingModel 缺 model 字段 → 400。
func TestGatewayMissingModel(t *testing.T) {
	g, _, plain := newTestGateway(t, nil)
	w := postChat(t, g, plain, []byte(`{"messages":[]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 model 应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestGatewayBodyTooLarge pre-call 请求体限制（运行轨热更键生效）。
func TestGatewayBodyTooLarge(t *testing.T) {
	g, _, plain := newTestGateway(t, map[string]string{OptionKeyMaxBodyBytes: "16"})
	w := postChat(t, g, plain, []byte(`{"model":"m1","pad":"`+strings.Repeat("x", 64)+`"}`))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("应 413，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestGatewayRetryOnServerError Server 错误立即换点：高优先级 500 → 低优先级 200。
func TestGatewayRetryOnServerError(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.HasPrefix(r.URL.Path, "/bad") {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	bad := seedChannel(t, st, model.Channel{Name: "bad", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL + "/bad", Key: "k1", Models: "m1", Priority: 10})
	seedChannel(t, st, model.Channel{Name: "good", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL + "/good", Key: "k2", Models: "m1", Priority: 1})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("换点后应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(*hits) != 2 {
		t.Fatalf("上游应收到 2 次请求（500→200），实际 %d", len(*hits))
	}
	if !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Fatalf("应为成功渠道的响应: %s", w.Body.String())
	}
	// 失败渠道进入熔断计数（1 次 Server 失败未达阈值，但计数应记录）。
	r := g.Breaker()
	b := r.get(bad.ID)
	b.mu.Lock()
	fails := b.fails
	b.mu.Unlock()
	if fails != 1 {
		t.Fatalf("bad 渠道应累计 1 次失败，实际 %d", fails)
	}
}

// TestGatewayAuthClassNoRetry Auth 类零重试：401 → 502 语义包装，且只打一次上游。
func TestGatewayAuthClassNoRetry(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	})
	seedChannel(t, st, model.Channel{Name: "a1", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "wrong", Models: "m1", Priority: 10})
	seedChannel(t, st, model.Channel{Name: "a2", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "wrong", Models: "m1", Priority: 5})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[]}`))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("Auth 类应 502，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_auth_failed") {
		t.Fatalf("错误码应为 upstream_auth_failed: %s", w.Body.String())
	}
	if len(*hits) != 1 {
		t.Fatalf("Auth 零重试：上游只应收到 1 次请求，实际 %d", len(*hits))
	}
}

// TestGatewayRateLimitRetry 429 指数退避换点重试（sleep 注入验证退避发生）。
func TestGatewayRateLimitRetry(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)

	var sleeps []time.Duration
	oldSleep := sleepFunc
	sleepFunc = func(d time.Duration) { sleeps = append(sleeps, d) }
	defer func() { sleepFunc = oldSleep }()

	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.HasPrefix(r.URL.Path, "/limited") {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	seedChannel(t, st, model.Channel{Name: "limited", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL + "/limited", Key: "k1", Models: "m1", Priority: 10})
	seedChannel(t, st, model.Channel{Name: "good", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL + "/good", Key: "k2", Models: "m1", Priority: 1})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("换点后应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(*hits) != 2 {
		t.Fatalf("上游应收到 2 次请求（429→200），实际 %d", len(*hits))
	}
	if len(sleeps) != 1 || sleeps[0] != RateLimitBackoff(0) {
		t.Fatalf("429 应触发一次指数退避: %v", sleeps)
	}
}

// TestGatewayOverrideApplied 渠道级 param_override 作用于上游请求体（set/delete/append）。
func TestGatewayOverrideApplied(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	var gotBody []byte
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		gotBody = body
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	seedChannel(t, st, model.Channel{
		Name: "ov", Type: model.ChannelTypeOpenAICompatible, BaseURL: up.URL, Key: "k", Models: "m1",
		ParamOverride: `{"set":{"temperature":0.5,"metadata.tag":"v2"},"delete":["top_p"],"append":{"messages.0.content":"(改写)"}}`,
	})

	w := postChat(t, g, plain, []byte(`{"model":"m1","top_p":0.9,"temperature":1,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("解析上游收到的请求体失败: %v: %s", err, gotBody)
	}
	if body["temperature"] != 0.5 {
		t.Fatalf("temperature 应被 set 为 0.5: %v", body["temperature"])
	}
	if _, ok := body["top_p"]; ok {
		t.Fatal("top_p 应被 delete")
	}
	msgs, _ := body["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if m0["content"] != "hi(改写)" {
		t.Fatalf("append 应拼接内容: %v", m0["content"])
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta["tag"] != "v2" {
		t.Fatalf("嵌套 set 应自动建路径: %v", body["metadata"])
	}
}

// TestGatewayMaxExcluded 排除集上限 5：6 个全故障渠道依次尝试，第 6 次失败后透传。
func TestGatewayMaxExcluded(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	up, hits := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.Error(w, `{"error":"down"}`, http.StatusInternalServerError)
	})
	for i := 6; i >= 1; i-- {
		seedChannel(t, st, model.Channel{
			Name: fmt.Sprintf("c%d", i), Type: model.ChannelTypeOpenAICompatible,
			BaseURL: up.URL, Key: "k", Models: "m1", Priority: int64(i),
		})
	}

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[]}`))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("排除集穷尽应透传上游 500，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(*hits) != 6 {
		t.Fatalf("首发 + 5 次换点 = 6 次上游请求，实际 %d", len(*hits))
	}
}

// TestGatewayNetworkError 上游不可达：重试穷尽（排除后无渠道）→ 503。
func TestGatewayNetworkError(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	// 占用一个本地端口后立即关闭：对该地址的连接必然被拒。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用本地端口失败: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	seedChannel(t, st, model.Channel{Name: "dead", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "http://" + addr, Key: "k", Models: "m1"})

	w := postChat(t, g, plain, []byte(`{"model":"m1","messages":[]}`))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("重试穷尽后应 503，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestGatewayAnthropicMessages Anthropic 协议经编排：双鉴权头、版本透传、路径与转发。
func TestGatewayAnthropicMessages(t *testing.T) {
	g, st, plain := newTestGateway(t, nil)
	var gotAuth, gotVersion, gotPath string
	up, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		gotAuth = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":20,"cache_read_input_tokens":5,"output_tokens":8}}`))
	})
	seedChannel(t, st, model.Channel{Name: "ap", Type: model.ChannelTypeAnthropic,
		BaseURL: up.URL, Key: "sk-ant-channel", Models: "claude-x"})

	c, w := ginCtx(t, http.MethodPost, "/v1/messages",
		[]byte(`{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		// Header 字面量 key 需用 canonical 形式（Get 按 canonical 查找；服务端会自动 canonical 化）。
		http.Header{"X-Api-Key": []string{plain}, "Anthropic-Version": []string{"2023-01-01"}})
	g.Serve(c, anthropic.New())

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if gotAuth != "sk-ant-channel" {
		t.Fatalf("上游 x-api-key 应为渠道密钥: %q", gotAuth)
	}
	if gotVersion != "2023-01-01" {
		t.Fatalf("anthropic-version 应透传: %q", gotVersion)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("上游路径不符: %s", gotPath)
	}
	if !strings.Contains(w.Body.String(), `"text":"ok"`) {
		t.Fatalf("响应体应透传: %s", w.Body.String())
	}
}

// TestHandleModels 模型并集 + 虚拟组名（组内不展开）+ 通配渠道不展开 + 鉴权。
func TestHandleModels(t *testing.T) {
	g, st, plain := newTestGateway(t, map[string]string{
		OptionKeyVirtualGroups: `{"团队A":["m1","m2"]}`,
	})
	seedChannel(t, st, model.Channel{Name: "a", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://a", Models: "m1,m2"})
	seedChannel(t, st, model.Channel{Name: "b", Type: model.ChannelTypeAnthropic,
		BaseURL: "https://b", Models: "m2,m3"})
	seedChannel(t, st, model.Channel{Name: "c", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://c", Models: "*"})

	// 无 key → 401。
	c, w := ginCtx(t, http.MethodGet, "/v1/models", nil, http.Header{})
	g.HandleModels(c)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，实际 %d", w.Code)
	}

	// 有效 key → 并集 + 组名。
	c, w = ginCtx(t, http.MethodGet, "/v1/models", nil,
		http.Header{"Authorization": []string{"Bearer " + plain}})
	g.HandleModels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 /v1/models 响应失败: %v", err)
	}
	if out.Object != "list" {
		t.Fatalf("object 应为 list: %q", out.Object)
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	want := []string{"m1", "m2", "m3", "团队A"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("模型清单应为 %v（通配渠道不展开、组名不展开成员），实际 %v", want, ids)
	}
}
