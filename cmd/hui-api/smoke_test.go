package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// openaiSSE / anthropicSSE 是假上游的流式响应原文（含 usage 与终止事件）。
const openaiSSE = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

const anthropicSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":5}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"嗨\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// newFakeUpstream 构造同时提供 OpenAI 与 Anthropic 两种上游形态的假上游。
func newFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer sk-up-oa" {
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = io.WriteString(w, openaiSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`))
	})
	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":21}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("x-api-key") != "sk-up-an" {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = io.WriteString(w, anthropicSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":20,"cache_read_input_tokens":5,"output_tokens":7}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// smokePost 向本地服务发 JSON POST。
func smokePost(t *testing.T, client *http.Client, url string, header http.Header, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestSmokeDualProtocol 端到端冒烟：完整路由 + 一个 httptest 假上游 + 本地起服务，
// 一次覆盖 OpenAI 与 Anthropic 两协议的非流式与流式完整请求、count_tokens 与 /v1/models。
func TestSmokeDualProtocol(t *testing.T) {
	up := newFakeUpstream(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "smoke.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	schemaVersion, err := st.Migrate()
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	plain := "sk-smoke-e2e-0001"
	if err := st.Write.Create(&model.Token{UserID: 1, Name: "smoke", Key: plain,
		KeyHash: gateway.HashKey(plain), Status: model.StatusEnabled,
		ExpiredTime: model.EpochForever, CreatedTime: time.Now().Unix()}).Error; err != nil {
		t.Fatalf("写入令牌失败: %v", err)
	}
	now := time.Now().Unix()
	if err := st.Write.Create(&model.Channel{Name: "oa", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: up.URL, Key: "sk-up-oa", Models: "gpt-smoke", Status: model.StatusEnabled,
		CreatedTime: now, UpdatedTime: now}).Error; err != nil {
		t.Fatalf("写入 OpenAI 渠道失败: %v", err)
	}
	if err := st.Write.Create(&model.Channel{Name: "an", Type: model.ChannelTypeAnthropic,
		BaseURL: up.URL, Key: "sk-up-an", Models: "claude-smoke", Status: model.StatusEnabled,
		CreatedTime: now, UpdatedTime: now}).Error; err != nil {
		t.Fatalf("写入 Anthropic 渠道失败: %v", err)
	}

	rt, err := config.NewRuntime(st)
	if err != nil {
		t.Fatalf("构造运行轨失败: %v", err)
	}

	// 本地起服务（与 run() 相同的完整路由）。
	srv := httptest.NewServer(newRouter(st, rt, schemaVersion))
	t.Cleanup(srv.Close)
	base := srv.URL
	client := &http.Client{}
	auth := http.Header{"Authorization": []string{"Bearer " + plain}}

	// ---- OpenAI 非流式 ----
	resp := smokePost(t, client, base+"/v1/chat/completions", auth,
		`{"model":"gpt-smoke","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("OpenAI 非流式应 200，实际 %d body=%s", resp.StatusCode, b)
	}
	var oa struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&oa); err != nil {
		t.Fatalf("解析 OpenAI 非流式响应失败: %v", err)
	}
	if len(oa.Choices) != 1 || oa.Choices[0].Message.Content != "hi" {
		t.Fatalf("OpenAI 非流式内容不符: %+v", oa)
	}
	if oa.Usage.PromptTokens != 11 || oa.Usage.CompletionTokens != 2 {
		t.Fatalf("OpenAI usage 不符: %+v", oa.Usage)
	}

	// ---- OpenAI 流式 ----
	resp = smokePost(t, client, base+"/v1/chat/completions", auth,
		`{"model":"gpt-smoke","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("OpenAI 流式应 200，实际 %d body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("流式 Content-Type 应为 SSE: %q", ct)
	}
	sseBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(sseBody), "[DONE]") || !strings.Contains(string(sseBody), `"content":"好"`) {
		t.Fatalf("OpenAI 流式响应不完整: %q", sseBody)
	}

	// ---- Anthropic 非流式（x-api-key 鉴权）----
	anHeader := http.Header{"X-Api-Key": []string{plain}, "Anthropic-Version": []string{"2023-06-01"}}
	resp = smokePost(t, client, base+"/v1/messages", anHeader,
		`{"model":"claude-smoke","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("Anthropic 非流式应 200，实际 %d body=%s", resp.StatusCode, b)
	}
	var an struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			OutputTokens         int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&an); err != nil {
		t.Fatalf("解析 Anthropic 非流式响应失败: %v", err)
	}
	if len(an.Content) != 1 || an.Content[0].Text != "ok" {
		t.Fatalf("Anthropic 非流式内容不符: %+v", an)
	}
	if an.Usage.InputTokens != 20 || an.Usage.CacheReadInputTokens != 5 || an.Usage.OutputTokens != 7 {
		t.Fatalf("Anthropic usage 不符: %+v", an.Usage)
	}

	// ---- Anthropic 流式 ----
	resp = smokePost(t, client, base+"/v1/messages", anHeader,
		`{"model":"claude-smoke","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("Anthropic 流式应 200，实际 %d body=%s", resp.StatusCode, b)
	}
	sseBody, _ = io.ReadAll(resp.Body)
	sse := string(sseBody)
	for _, want := range []string{"message_start", "content_block_delta", `"text":"嗨"`, "message_delta", "message_stop"} {
		if !strings.Contains(sse, want) {
			t.Fatalf("Anthropic 流式响应缺少 %s: %q", want, sse)
		}
	}

	// ---- count_tokens 透传 ----
	resp = smokePost(t, client, base+"/v1/messages/count_tokens", anHeader,
		`{"model":"claude-smoke","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("count_tokens 应 200，实际 %d body=%s", resp.StatusCode, b)
	}
	var ct struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ct); err != nil || ct.InputTokens != 21 {
		t.Fatalf("count_tokens 应透传 input_tokens=21: %v %+v", err, ct)
	}

	// ---- /v1/models 并集 ----
	reqM, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		t.Fatalf("构造 models 请求失败: %v", err)
	}
	reqM.Header.Set("Authorization", "Bearer "+plain)
	respM, err := client.Do(reqM)
	if err != nil {
		t.Fatalf("请求 /v1/models 失败: %v", err)
	}
	defer func() { _ = respM.Body.Close() }()
	if respM.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respM.Body)
		t.Fatalf("/v1/models 应 200，实际 %d body=%s", respM.StatusCode, b)
	}
	modelsBody, _ := io.ReadAll(respM.Body)
	modelsText := string(modelsBody)
	for _, want := range []string{"gpt-smoke", "claude-smoke"} {
		if !strings.Contains(modelsText, want) {
			t.Fatalf("/v1/models 缺少 %s: %s", want, modelsText)
		}
	}

	t.Log("冒烟通过：OpenAI 非流式/流式、Anthropic 非流式/流式、count_tokens、/v1/models")
}
