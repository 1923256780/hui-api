// Package openai 实现 OpenAI 兼容协议面（docs/01 设计点 2）：
// POST /v1/chat/completions 的请求解析、上游请求构造（流式注入
// stream_options.include_usage）、SSE 逐事件转发与非流式透传、usage 提取。
// 本包只做协议适配，不做路由/重试/熔断等业务决策（见 internal/gateway）。
package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/relay"
)

// errBadBody 是请求体非法的本地哨兵。
var errBadBody = errors.New("invalid request body")

// UpstreamChatPath 是上游对话补全 API 路径（base_url 拼接规则见 relay.JoinBaseURL）。
const UpstreamChatPath = "/v1/chat/completions"

// Protocol 实现 relay.Protocol（OpenAI 兼容）。
type Protocol struct{}

// New 构造 OpenAI 协议适配器。
func New() *Protocol { return &Protocol{} }

// Name 返回协议面名称。
func (p *Protocol) Name() string { return relay.ProtoOpenAI }

// ChannelType 返回可服务的渠道类型。
func (p *Protocol) ChannelType() int { return model.ChannelTypeOpenAICompatible }

// ExtractKey 提取 Authorization: Bearer <key>。
func (p *Protocol) ExtractKey(c *gin.Context) (string, bool) {
	h := c.Request.Header.Get("Authorization")
	key, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(key) == "" {
		return "", false
	}
	return strings.TrimSpace(key), true
}

// ParseBody 解析请求体：模型名与流式标记。
func (p *Protocol) ParseBody(raw []byte) (*relay.ParsedRequest, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadBody, err)
	}
	pr := &relay.ParsedRequest{Body: body}
	if m, ok := body["model"].(string); ok {
		pr.Model = m
	}
	if s, ok := body["stream"].(bool); ok {
		pr.Stream = s
	}
	return pr, nil
}

// UpstreamPath 返回上游 API 路径。
func (p *Protocol) UpstreamPath(*gin.Context) string { return UpstreamChatPath }

// PrepareUpstream 构造上游请求：
//   - URL = base_url 拼接 /v1/chat/completions（尾斜杠与 /v1 归一）；
//   - 流式时注入 stream_options.include_usage=true（取尾 chunk usage；
//     上游返回原文透传，客户端会多收到一个 usage chunk）；
//   - 鉴权头改用渠道密钥（Authorization: Bearer <channel.key>）。
func (p *Protocol) PrepareUpstream(ch *model.Channel, apiPath string, payload []byte, inHeader http.Header) (*http.Request, error) {
	body := payload
	if isStreamPayload(payload) {
		var err error
		if body, err = injectIncludeUsage(payload); err != nil {
			return nil, err
		}
	}
	req, err := relay.NewUpstreamRequest(http.MethodPost, relay.JoinBaseURL(ch.BaseURL, apiPath), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+ch.Key)
	return req, nil
}

// Respond 把上游 2xx 响应转发给客户端并提取 usage。
func (p *Protocol) Respond(c *gin.Context, resp *http.Response, pr *relay.ParsedRequest) (relay.Usage, error) {
	defer func() { _ = resp.Body.Close() }()

	if pr.Stream {
		var usage relay.Usage
		_, err := relay.ForwardStream(c, resp.Body, func(data []byte) {
			u := parseChunkUsage(data)
			if u != nil {
				usage = *u
			}
		})
		return usage, err
	}

	// 非流式：整体读取透传（客户端 SDK 按整体 JSON 解析）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, relay.MaxRespBody))
	if err != nil {
		return relay.Usage{}, fmt.Errorf("读取上游响应: %w", err)
	}
	usage := parseNonStreamUsage(raw)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(http.StatusOK, contentType, raw)
	return usage, nil
}

// WriteError 按入口协议错误形状输出（OpenAI error object，客户端 SDK 无感）。
func (p *Protocol) WriteError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}

// isStreamPayload 判断请求体是否流式（stream: true）。
func isStreamPayload(payload []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// injectIncludeUsage 注入 stream_options.include_usage=true。
// 若客户端已带 stream_options，仅强制 include_usage；否则补全该对象。
func injectIncludeUsage(payload []byte) ([]byte, error) {
	var body map[string]any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("解析流式请求体: %w", err)
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok {
		so = map[string]any{}
		body["stream_options"] = so
	}
	so["include_usage"] = true
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化注入后请求体: %w", err)
	}
	return out, nil
}

// parseChunkUsage 解析流式单个 data 块的 usage（尾 chunk 才携带；无 usage 返回 nil）。
func parseChunkUsage(data []byte) *relay.Usage {
	if len(data) == 0 || data[0] != '{' {
		return nil // [DONE] 等非 JSON 行
	}
	var chunk struct {
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil || chunk.Usage == nil {
		return nil
	}
	return chunk.Usage.toUsage()
}

// parseNonStreamUsage 解析非流式响应的 usage 字段。
func parseNonStreamUsage(raw []byte) relay.Usage {
	var resp struct {
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Usage == nil {
		return relay.Usage{}
	}
	u := resp.Usage.toUsage()
	return *u
}

// wireUsage 是 OpenAI usage 字段的线格式（含缓存读计数）。
type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	PromptTokensDet  *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// toUsage 归一为 relay.Usage；缓存读部分计入 PromptTokens 并单独标注。
func (w *wireUsage) toUsage() *relay.Usage {
	u := &relay.Usage{PromptTokens: w.PromptTokens, CompletionTokens: w.CompletionTokens}
	if w.PromptTokensDet != nil {
		u.CacheReadTokens = w.PromptTokensDet.CachedTokens
	}
	return u
}
