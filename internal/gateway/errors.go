// Package gateway 是转发链路的路由编排层（docs/01 设计点 2/3）：令牌鉴权、
// Deployment（渠道）选择、熔断状态机、typed retry 与 pre-call 检查都在这里
// 做业务决策；协议适配在 internal/relay/<protocol>，适配层不感知本包细节。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrClass 是上游/网络错误的类型分类，驱动重试策略表（docs/05 typed retry）。
type ErrClass int

const (
	ClassUnknown       ErrClass = iota
	ClassAuth                   // 上游 401/403：渠道密钥无效
	ClassRateLimit              // 上游 429：上游限流
	ClassTimeout                // 网络超时 / 连接中断
	ClassBadRequest             // 上游 400：请求本身不合法
	ClassContextWindow          // 上下文超限（413 或上下文长度语义）
	ClassContentPolicy          // 内容策略拒绝
	ClassServer                 // 上游 5xx
)

// String 实现 fmt.Stringer（日志输出用）。
func (c ErrClass) String() string {
	switch c {
	case ClassAuth:
		return "auth"
	case ClassRateLimit:
		return "rate_limit"
	case ClassTimeout:
		return "timeout"
	case ClassBadRequest:
		return "bad_request"
	case ClassContextWindow:
		return "context_window"
	case ClassContentPolicy:
		return "content_policy"
	case ClassServer:
		return "server"
	default:
		return "unknown"
	}
}

// TypedError 是分类后的上游错误。
type TypedError struct {
	Class  ErrClass
	Status int    // 上游 HTTP 状态码；0 表示网络层错误（未拿到响应）
	Body   []byte // 上游错误响应体（透传用）；网络错误时为空
	Msg    string // 简述（日志用）
}

// Error 实现 error。
func (e *TypedError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("上游错误 class=%s status=%d: %s", e.Class, e.Status, e.Msg)
	}
	return fmt.Sprintf("上游错误 class=%s: %s", e.Class, e.Msg)
}

// ErrBodyLimit 是读取上游错误响应体的字节上限（错误体不会很大，防异常上游）。
const ErrBodyLimit = 64 << 10

// ClassifyStatus 把上游响应状态码分类为 ErrClass。
// body 仅用于识别 ContextWindow / ContentPolicy 等需语义判断的场景。
func ClassifyStatus(status int, body []byte) ErrClass {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ClassAuth
	case status == http.StatusBadRequest:
		if containsAny(lower(body),
			"context length", "context_length", "max_tokens", "too long", "too many tokens") {
			return ClassContextWindow
		}
		if containsAny(lower(body), "content_policy", "content policy", "safety", "moderation") {
			return ClassContentPolicy
		}
		return ClassBadRequest
	case status == http.StatusRequestEntityTooLarge:
		return ClassContextWindow
	case status >= 500:
		return ClassServer
	default:
		return ClassUnknown
	}
}

// ClassifyNetworkErr 把网络层错误分类（超时 / 连接中断 / 其它）。
func ClassifyNetworkErr(err error) ErrClass {
	if err == nil {
		return ClassUnknown
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return ClassTimeout
	}
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") || strings.Contains(msg, "refused") {
		return ClassTimeout
	}
	return ClassUnknown
}

func lower(b []byte) string { return strings.ToLower(string(b)) }

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// 预调用/pre-call 语义错误（docs/05：无渠道返回 503 语义错误）。
var (
	// ErrNoChannel 表示模型在全部候选渠道中无可用 deployment（含全部熔断中）。
	ErrNoChannel = errors.New("no available channel for model")
	// ErrBodyTooLarge 表示请求体超过本地限制。
	ErrBodyTooLarge = errors.New("request body too large")
	// ErrBadKey 表示未携带客户端密钥。
	ErrBadKey = errors.New("missing api key")
	// ErrBadModel 表示请求未指定模型。
	ErrBadModel = errors.New("missing model in request")
	// ErrBadBody 表示请求体非法。
	ErrBadBody = errors.New("invalid request body")
)

// sleepFunc 是可注入的退避等待（测试用零等待替身）。
var sleepFunc = time.Sleep
