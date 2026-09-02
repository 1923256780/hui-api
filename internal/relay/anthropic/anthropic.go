// Package anthropic 实现 Anthropic Messages 协议面（docs/01 设计点 2）：
// POST /v1/messages 的双鉴权（x-api-key 与 Bearer）、anthropic-version 透传、
// SSE 事件流转发、usage 提取（message_start 输入侧 + message_delta 累计输出侧），
// 以及 POST /v1/messages/count_tokens 透传。
// 本包只做协议适配，不做路由/重试/熔断等业务决策（见 internal/gateway）。
package anthropic

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

// 上游 API 路径。
const (
	UpstreamMessagesPath    = "/v1/messages"
	UpstreamCountTokensPath = "/v1/messages/count_tokens"
	// EntryCountTokensPath 是入口 count_tokens 路径（与上游同形）。
	EntryCountTokensPath = "/v1/messages/count_tokens"
)

// AnthropicVersionHeader 是协议版本头名；客户端未携带时注入该默认值。
const (
	AnthropicVersionHeader = "anthropic-version"
	DefaultVersion         = "2023-06-01"
)

// errBadBody 是请求体非法的本地哨兵。
var errBadBody = errors.New("invalid request body")

// Protocol 实现 relay.Protocol（Anthropic Messages）。
type Protocol struct{}

// New 构造 Anthropic 协议适配器。
func New() *Protocol { return &Protocol{} }

// Name 返回协议面名称。
func (p *Protocol) Name() string { return relay.ProtoAnthropic }

// ChannelType 返回可服务的渠道类型。
func (p *Protocol) ChannelType() int { return model.ChannelTypeAnthropic }

// ExtractKey 双鉴权：优先 x-api-key，其次 Authorization: Bearer（docs/05）。
func (p *Protocol) ExtractKey(c *gin.Context) (string, bool) {
	if key := strings.TrimSpace(c.Request.Header.Get("x-api-key")); key != "" {
		return key, true
	}
	if key, ok := strings.CutPrefix(c.Request.Header.Get("Authorization"), "Bearer "); ok {
		if key = strings.TrimSpace(key); key != "" {
			return key, true
		}
	}
	return "", false
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

// UpstreamPath 依据入口路径区分 messages 与 count_tokens。
func (p *Protocol) UpstreamPath(c *gin.Context) string {
	if c.FullPath() == EntryCountTokensPath {
		return UpstreamCountTokensPath
	}
	return UpstreamMessagesPath
}

// PrepareUpstream 构造上游请求：
//   - URL = base_url 拼接 /v1/messages（或 /v1/messages/count_tokens）；
//   - 鉴权：x-api-key: <channel.key>（Anthropic 标准头）；
//   - anthropic-version 透传（客户端未携带时注入默认版本）；
//   - count_tokens 恒为非流式，无需注入。
func (p *Protocol) PrepareUpstream(ch *model.Channel, apiPath string, payload []byte, inHeader http.Header) (*http.Request, error) {
	req, err := relay.NewUpstreamRequest(http.MethodPost, relay.JoinBaseURL(ch.BaseURL, apiPath), strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", ch.Key)
	version := inHeader.Get(AnthropicVersionHeader)
	if version == "" {
		version = DefaultVersion
	}
	req.Header.Set(AnthropicVersionHeader, version)
	return req, nil
}

// Respond 把上游 2xx 响应转发给客户端并提取 usage。
func (p *Protocol) Respond(c *gin.Context, resp *http.Response, pr *relay.ParsedRequest) (relay.Usage, error) {
	defer func() { _ = resp.Body.Close() }()

	if pr.CountTokens {
		// count_tokens：非流式 JSON，input_tokens 即输入侧用量。
		raw, err := io.ReadAll(io.LimitReader(resp.Body, relay.MaxRespBody))
		if err != nil {
			return relay.Usage{}, fmt.Errorf("读取上游响应: %w", err)
		}
		usage := relay.Usage{}
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(raw, &out); err == nil {
			usage.PromptTokens = out.InputTokens
		}
		c.Data(http.StatusOK, resp.Header.Get("Content-Type"), raw)
		return usage, nil
	}

	if pr.Stream {
		var usage relay.Usage
		_, err := relay.ForwardStream(c, resp.Body, func(data []byte) {
			parseEventUsage(data, &usage)
		})
		return usage, err
	}

	// 非流式 messages 响应：usage 字段直取。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, relay.MaxRespBody))
	if err != nil {
		return relay.Usage{}, fmt.Errorf("读取上游响应: %w", err)
	}
	usage := relay.Usage{}
	var out struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			OutputTokens         int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err == nil {
		usage.PromptTokens = out.Usage.InputTokens + out.Usage.CacheReadInputTokens
		usage.CacheReadTokens = out.Usage.CacheReadInputTokens
		usage.CompletionTokens = out.Usage.OutputTokens
	}
	c.Data(http.StatusOK, resp.Header.Get("Content-Type"), raw)
	return usage, nil
}

// WriteError 按入口协议错误形状输出（Anthropic error object，客户端 SDK 无感）。
func (p *Protocol) WriteError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// parseEventUsage 从 SSE 事件 data 中累计 usage（Anthropic 流式语义）：
//   - message_start：message.usage.input_tokens（输入侧，含 cache_read_input_tokens）；
//   - message_delta：usage.output_tokens 为累计值，直接覆盖。
//
// 返回是否发生更新。
func parseEventUsage(data []byte, acc *relay.Usage) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage struct {
				InputTokens          int `json:"input_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return false
	}
	updated := false
	switch {
	case ev.Type == "message_start" && ev.Message != nil:
		u := ev.Message.Usage
		acc.PromptTokens = u.InputTokens + u.CacheReadInputTokens
		acc.CacheReadTokens = u.CacheReadInputTokens
		updated = true
	case ev.Type == "message_delta" && ev.Usage != nil:
		acc.CompletionTokens = ev.Usage.OutputTokens // 累计值语义：直接覆盖
		updated = true
	}
	return updated
}
