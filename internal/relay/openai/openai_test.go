package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/relay"
)

func testGinContext(w http.ResponseWriter) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	return c
}

// TestExtractKey 仅接受 Authorization: Bearer。
func TestExtractKey(t *testing.T) {
	p := New()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	if _, ok := p.ExtractKey(c); ok {
		t.Fatal("无鉴权头应返回 false")
	}
	c.Request.Header.Set("Authorization", "sk-plain")
	if _, ok := p.ExtractKey(c); ok {
		t.Fatal("非 Bearer 前缀应返回 false")
	}
	c.Request.Header.Set("Authorization", "Bearer sk-test-123")
	got, ok := p.ExtractKey(c)
	if !ok || got != "sk-test-123" {
		t.Fatalf("Bearer 提取不符: %q %v", got, ok)
	}
}

// TestParseBody 模型与流式标记解析。
func TestParseBody(t *testing.T) {
	p := New()
	pr, err := p.ParseBody([]byte(`{"model":"m1","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("ParseBody 失败: %v", err)
	}
	if pr.Model != "m1" || !pr.Stream {
		t.Fatalf("解析结果不符: %+v", pr)
	}
	if _, err := p.ParseBody([]byte(`bad`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestInjectIncludeUsage 流式注入 stream_options.include_usage。
func TestInjectIncludeUsage(t *testing.T) {
	// 无 stream_options：补全对象。
	out, err := injectIncludeUsage([]byte(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	so, ok := got["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("应注入 include_usage=true: %v", got["stream_options"])
	}
	// 已有 stream_options：强制 include_usage=true。
	out, err = injectIncludeUsage([]byte(`{"stream":true,"stream_options":{"include_usage":false}}`))
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	_ = json.Unmarshal(out, &got)
	so = got["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("已配置 false 时应强制为 true: %v", so)
	}
}

// TestPrepareUpstream 构造：URL 拼接 + 渠道密钥鉴权 + 非流式不注入。
func TestPrepareUpstream(t *testing.T) {
	p := New()
	ch := &model.Channel{BaseURL: "https://up.example", Key: "up-key"}
	req, err := p.PrepareUpstream(ch, UpstreamChatPath, []byte(`{"model":"m","stream":false}`), http.Header{})
	if err != nil {
		t.Fatalf("PrepareUpstream 失败: %v", err)
	}
	if req.URL.String() != "https://up.example/v1/chat/completions" {
		t.Fatalf("URL 拼接不符: %s", req.URL)
	}
	if req.Header.Get("Authorization") != "Bearer up-key" {
		t.Fatalf("鉴权头应用渠道密钥: %q", req.Header.Get("Authorization"))
	}
	if strings.Contains(string(readBody(t, req)), "stream_options") {
		t.Fatal("非流式不应注入 stream_options")
	}
}

// TestPrepareUpstreamStreamInject 流式时注入 include_usage。
func TestPrepareUpstreamStreamInject(t *testing.T) {
	p := New()
	ch := &model.Channel{BaseURL: "https://up.example/v1/", Key: "k"}
	req, err := p.PrepareUpstream(ch, UpstreamChatPath, []byte(`{"model":"m","stream":true}`), http.Header{})
	if err != nil {
		t.Fatalf("PrepareUpstream 失败: %v", err)
	}
	if req.URL.String() != "https://up.example/v1/chat/completions" {
		t.Fatalf("URL 拼接不符: %s", req.URL)
	}
	body := readBody(t, req)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	so, ok := got["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("流式请求应注入 include_usage: %v", body)
	}
}

// TestRespondNonStream 非流式：原文透传 + usage 提取（含缓存读）。
func TestRespondNonStream(t *testing.T) {
	p := New()
	upstreamBody := `{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("请求假上游失败: %v", err)
	}
	rec := httptest.NewRecorder()
	usage, err := p.Respond(testGinContext(rec), resp, &relay.ParsedRequest{})
	if err != nil {
		t.Fatalf("Respond 失败: %v", err)
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("非流式应原文透传: %q", rec.Body.String())
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.CacheReadTokens != 40 {
		t.Fatalf("usage 提取不符: %+v", usage)
	}
}

// TestRespondStream 流式：逐事件原文转发 + 尾 chunk usage 提取。
func TestRespondStream(t *testing.T) {
	p := New()
	events := []string{
		`data: {"choices":[{"delta":{"content":"he"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, e := range events {
			_, _ = fmt.Fprintln(w, e)
			if strings.TrimSpace(e) == "" {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("请求假上游失败: %v", err)
	}
	rec := httptest.NewRecorder()
	usage, err := p.Respond(testGinContext(rec), resp, &relay.ParsedRequest{Stream: true})
	if err != nil {
		t.Fatalf("Respond 失败: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `{"choices":[{"delta":{"content":"he"}}]}`) ||
		!strings.Contains(body, "[DONE]") {
		t.Fatalf("流式应原文透传事件: %q", body)
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("尾 chunk usage 提取不符: %+v", usage)
	}
}

// TestWriteError 错误形状：OpenAI error object。
func TestWriteError(t *testing.T) {
	p := New()
	rec := httptest.NewRecorder()
	p.WriteError(testGinContext(rec), http.StatusUnauthorized, "invalid_api_key", "密钥无效")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码不符: %d", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if body.Error.Type != "invalid_api_key" || body.Error.Message != "密钥无效" {
		t.Fatalf("错误形状不符: %+v", body.Error)
	}
}

// readBody 读取 req.Body 并复位（测试辅助）。
func readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	if req.Body == nil {
		return nil
	}
	buf := make([]byte, req.ContentLength+1)
	n, _ := req.Body.Read(buf)
	return buf[:n]
}
