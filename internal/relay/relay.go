// Package relay 是双协议转发核心（docs/01 设计点 2）：对外暴露 OpenAI 兼容与
// Anthropic Messages 两种协议面，协议适配与转发编排解耦——本包只定义协议适配
// 接口与公共工具（SSE 逐事件转发、URL 拼接、用量归一），不做任何业务决策；
// 编排（鉴权、渠道选择、熔断、重试）在 internal/gateway。
package relay

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
)

// 协议面名称（日志与匹配用）。
const (
	ProtoOpenAI    = "openai"
	ProtoAnthropic = "anthropic"
)

// SSEContentType 是流式响应的固定 Content-Type。
const SSEContentType = "text/event-stream; charset=utf-8"

// MaxRespBody 是非流式上游响应体的读取上限（防异常上游撑爆内存）。
const MaxRespBody = 64 << 20

// Usage 是跨协议归一后的用量（计费与日志的统一入参）。
// CacheReadTokens 是提示缓存读取部分，已计入 PromptTokens。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
}

// ParsedRequest 是请求体解析结果（协议归一描述）。
type ParsedRequest struct {
	Model       string         // 模型名（路由依据）
	Stream      bool           // 是否流式
	CountTokens bool           // Anthropic count_tokens 端点（恒非流式）
	Body        map[string]any // 解析后的请求对象（供流式注入等使用）
}

// Protocol 是协议适配接口：由 openai / anthropic 子包实现，gateway 编排调用。
// 适配层不做业务决策（不选渠道、不重试、不熔断）。
type Protocol interface {
	// Name 返回协议面名称（relay.ProtoOpenAI / relay.ProtoAnthropic）。
	Name() string
	// ChannelType 返回本协议可服务的渠道类型（model.ChannelType*）。
	ChannelType() int
	// ExtractKey 从入口请求提取客户端密钥；未携带时返回 false。
	ExtractKey(c *gin.Context) (string, bool)
	// ParseBody 解析请求体（model、stream 标记）；非法 JSON 报错。
	ParseBody(raw []byte) (*ParsedRequest, error)
	// UpstreamPath 返回本请求对应的上游 API 路径（如 /v1/chat/completions）。
	UpstreamPath(c *gin.Context) string
	// PrepareUpstream 构造发往上游的 *http.Request：URL 由渠道 base_url 与
	// apiPath 拼接，鉴权头用渠道密钥，需要透传的入口头由实现决定。
	// payload 是已经过 param_override 的最终请求体。
	PrepareUpstream(ch *model.Channel, apiPath string, payload []byte, inHeader http.Header) (*http.Request, error)
	// Respond 把上游 2xx 响应转发给客户端：流式逐事件 flush（不改写事件原文），
	// 非流式整体透传；同时提取 usage。调用后不可再重试（首字节已出）。
	Respond(c *gin.Context, resp *http.Response, pr *ParsedRequest) (Usage, error)
	// WriteError 按入口协议的错误形状输出错误（docs/05：客户端 SDK 无感）。
	WriteError(c *gin.Context, status int, errType, message string)
}

// JoinBaseURL 拼接渠道 base_url 与上游 API 路径。
// 规则：去尾部斜杠；base_url 已以 /v1 结尾时不重复拼 /v1 前缀。
// 例：https://a.com + /v1/chat/completions → https://a.com/v1/chat/completions；
//
//	https://a.com/v1 + /v1/chat/completions → https://a.com/v1/chat/completions。
func JoinBaseURL(baseURL, apiPath string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return apiPath
	}
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(apiPath, "/v1/") {
		return base + strings.TrimPrefix(apiPath, "/v1")
	}
	return base + apiPath
}

// NewUpstreamRequest 构造上游请求并校验目标地址：仅允许 http/https scheme。
// base_url 来自管理面配置（非终端用户输入），此处防误配置指向危险 scheme；
// 终端用户只能控制请求体，不能影响转发目标。
func NewUpstreamRequest(method, fullURL string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(fullURL)
	if err != nil {
		return nil, fmt.Errorf("解析上游地址 %s: %w", fullURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("上游地址 scheme 非法: %q", u.Scheme)
	}
	return http.NewRequest(method, fullURL, body)
}

// ForwardStream 把上游 SSE 响应体逐事件转发到 gin 响应（docs/01 设计点 2：
// 不缓冲整段、不改写事件原文），每遇到事件边界（空行）立即 Flush。
// onEvent 在每个 data 行被读出时回调（调用方可提取 usage；不得修改事件内容）。
// 返回转发总字节数；客户端断开时返回 nil（静默结束，上游由调用方关闭）。
func ForwardStream(c *gin.Context, body io.Reader, onEvent func(dataLine []byte)) (int, error) {
	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, fmt.Errorf("响应写入器不支持 Flush，无法流式转发")
	}

	w.Header().Set("Content-Type", SSEContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var total int
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	for scanner.Scan() {
		line := scanner.Bytes() // scanner 重用底层数组，写入前必须拷贝
		n, err := w.Write(append(line[:len(line):len(line)], '\n'))
		total += n
		if err != nil {
			return total, nil // 客户端断开
		}
		if isBlankLine(line) {
			flusher.Flush() // 事件边界：立即 flush
			continue
		}
		if onEvent != nil {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				onEvent(bytes.TrimSpace(data))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return total, fmt.Errorf("读取上游流: %w", err)
	}
	flusher.Flush()
	return total, nil
}

// maxEventLineSize 单行上限：SSE 事件行（一块增量文本 JSON）可能较大，给 8MB。
const maxEventLineSize = 8 << 20

// isBlankLine 判断是否 SSE 事件边界（空行；容忍尾部 \r）。
func isBlankLine(line []byte) bool {
	return len(line) == 0 || (len(line) == 1 && line[0] == '\r')
}

// passthroughHeaders 返回允许透传到上游的入口头子集（白名单制：
// 排除鉴权与逐跳头；转发面到上游的鉴权一律改用渠道密钥）。
func passthroughHeaders(in http.Header, allow []string) http.Header {
	out := make(http.Header, len(allow))
	for _, name := range allow {
		if v := in.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}
