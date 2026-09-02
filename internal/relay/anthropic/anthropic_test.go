package anthropic

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

// TestExtractKeyDual 双鉴权：x-api-key 优先，其次 Bearer。
func TestExtractKeyDual(t *testing.T) {
	p := New()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	if _, ok := p.ExtractKey(c); ok {
		t.Fatal("无鉴权头应返回 false")
	}
	c.Request.Header.Set("Authorization", "Bearer bearer-key")
	if got, ok := p.ExtractKey(c); !ok || got != "bearer-key" {
		t.Fatalf("Bearer 鉴权不符: %q %v", got, ok)
	}
	c.Request.Header.Set("x-api-key", "xkey-1")
	if got, ok := p.ExtractKey(c); !ok || got != "xkey-1" {
		t.Fatalf("x-api-key 应优先: %q %v", got, ok)
	}
}

// TestUpstreamPathCountTokens 入口路径映射上游路径。
func TestUpstreamPathCountTokens(t *testing.T) {
	p := New()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if got := p.UpstreamPath(c); got != UpstreamMessagesPath {
		t.Fatalf("messages 路径映射不符: %s", got)
	}
	// FullPath 由路由注册决定：直接构造带 FullPath 的上下文。
	r := gin.New()
	r.POST(EntryCountTokensPath, func(ctx *gin.Context) {
		if got := p.UpstreamPath(ctx); got != UpstreamCountTokensPath {
			t.Errorf("count_tokens 路径映射不符: %s", got)
		}
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EntryCountTokensPath, nil)
	r.ServeHTTP(w, req)
}

// TestPrepareUpstream x-api-key 鉴权 + anthropic-version 透传/默认注入。
func TestPrepareUpstream(t *testing.T) {
	p := New()
	ch := &model.Channel{BaseURL: "https://up.example", Key: "up-key"}

	// 客户端携带版本头：透传。
	in := http.Header{}
	in.Set(AnthropicVersionHeader, "2099-01-01")
	req, err := p.PrepareUpstream(ch, UpstreamMessagesPath, []byte(`{"model":"m"}`), in)
	if err != nil {
		t.Fatalf("PrepareUpstream 失败: %v", err)
	}
	if req.URL.String() != "https://up.example/v1/messages" {
		t.Fatalf("URL 拼接不符: %s", req.URL)
	}
	if req.Header.Get("x-api-key") != "up-key" {
		t.Fatalf("鉴权头应用渠道密钥: %q", req.Header.Get("x-api-key"))
	}
	if req.Header.Get(AnthropicVersionHeader) != "2099-01-01" {
		t.Fatalf("版本头应透传: %q", req.Header.Get(AnthropicVersionHeader))
	}

	// 客户端未携带：注入默认版本。
	req, err = p.PrepareUpstream(ch, UpstreamCountTokensPath, []byte(`{}`), http.Header{})
	if err != nil {
		t.Fatalf("PrepareUpstream 失败: %v", err)
	}
	if req.Header.Get(AnthropicVersionHeader) != DefaultVersion {
		t.Fatalf("未携带版本头应注入默认值: %q", req.Header.Get(AnthropicVersionHeader))
	}
	if req.URL.String() != "https://up.example/v1/messages/count_tokens" {
		t.Fatalf("count_tokens URL 拼接不符: %s", req.URL)
	}
}

// TestRespondStreamUsage 流式 usage 提取：message_start 输入侧（含缓存读）+
// message_delta 累计输出侧；事件原文不改写。
func TestRespondStreamUsage(t *testing.T) {
	p := New()
	events := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"cache_read_input_tokens":10,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
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
	if !strings.Contains(body, `"type":"message_start"`) || !strings.Contains(body, "content_block_delta") {
		t.Fatalf("事件原文应透传: %q", body)
	}
	if usage.PromptTokens != 35 { // 25 + cache_read 10
		t.Fatalf("input_tokens 应含 cache_read（25+10=35），实际 %d", usage.PromptTokens)
	}
	if usage.CacheReadTokens != 10 {
		t.Fatalf("cache_read 提取不符: %d", usage.CacheReadTokens)
	}
	if usage.CompletionTokens != 42 { // message_delta 累计值覆盖
		t.Fatalf("output_tokens 应取累计值 42，实际 %d", usage.CompletionTokens)
	}
}

// TestRespondCountTokens count_tokens 透传与 input_tokens 提取。
func TestRespondCountTokens(t *testing.T) {
	p := New()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":2095}`))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("请求假上游失败: %v", err)
	}
	rec := httptest.NewRecorder()
	usage, err := p.Respond(testGinContext(rec), resp, &relay.ParsedRequest{CountTokens: true})
	if err != nil {
		t.Fatalf("Respond 失败: %v", err)
	}
	if rec.Body.String() != `{"input_tokens":2095}` {
		t.Fatalf("count_tokens 应原文透传: %q", rec.Body.String())
	}
	if usage.PromptTokens != 2095 {
		t.Fatalf("input_tokens 提取不符: %d", usage.PromptTokens)
	}
}

// TestRespondNonStream 非流式 messages：usage 直取。
func TestRespondNonStream(t *testing.T) {
	p := New()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}],` +
			`"usage":{"input_tokens":12,"output_tokens":5}}`))
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
	if usage.PromptTokens != 12 || usage.CompletionTokens != 5 {
		t.Fatalf("非流式 usage 提取不符: %+v", usage)
	}
}

// TestWriteError 错误形状：Anthropic error object。
func TestWriteError(t *testing.T) {
	p := New()
	rec := httptest.NewRecorder()
	p.WriteError(testGinContext(rec), http.StatusUnauthorized, "authentication_error", "密钥无效")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码不符: %d", rec.Code)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if body.Type != "error" || body.Error.Type != "authentication_error" {
		t.Fatalf("错误形状不符: %+v", body)
	}
}
