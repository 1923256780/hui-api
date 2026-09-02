package gateway

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestPolicyFor 错误分类 → 重试策略表（docs/05 typed retry）：
// Auth 零重试；RateLimit 指数退避；其余立即换点。
func TestPolicyFor(t *testing.T) {
	cases := []struct {
		class     ErrClass
		retryable bool
		backoff   BackoffKind
	}{
		{ClassAuth, false, BackoffNone},
		{ClassRateLimit, true, BackoffExp},
		{ClassTimeout, true, BackoffNone},
		{ClassBadRequest, true, BackoffNone},
		{ClassContextWindow, true, BackoffNone},
		{ClassContentPolicy, true, BackoffNone},
		{ClassServer, true, BackoffNone},
		{ClassUnknown, true, BackoffNone},
	}
	for _, tc := range cases {
		p := PolicyFor(tc.class)
		if p.Retryable != tc.retryable || p.Backoff != tc.backoff {
			t.Fatalf("%s 策略不符: got %+v want retryable=%v backoff=%v", tc.class, p, tc.retryable, tc.backoff)
		}
	}
}

// TestRateLimitBackoff 指数退避序列：500ms 起倍增，封顶 4s。
func TestRateLimitBackoff(t *testing.T) {
	want := []time.Duration{
		500 * time.Millisecond, 1 * time.Second, 2 * time.Second,
		4 * time.Second, 4 * time.Second, 4 * time.Second,
	}
	for i, w := range want {
		if got := RateLimitBackoff(i); got != w {
			t.Fatalf("attempt=%d 退避应为 %s，实际 %s", i, w, got)
		}
	}
}

// TestClassifyStatus 状态码与语义分类（含 400 内的上下文超限与内容策略识别）。
func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrClass
	}{
		{429, "", ClassRateLimit},
		{401, "", ClassAuth},
		{403, "", ClassAuth},
		{400, `{"error":{"message":"context length exceeded"}}`, ClassContextWindow},
		{400, `{"error":{"message":"content policy violation"}}`, ClassContentPolicy},
		{400, `{"error":{"message":"invalid parameter"}}`, ClassBadRequest},
		{413, "", ClassContextWindow},
		{500, "", ClassServer},
		{503, "", ClassServer},
		{302, "", ClassUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyStatus(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("status=%d body=%q 应为 %s，实际 %s", tc.status, tc.body, tc.want, got)
		}
	}
}

// TestClassifyNetworkErr 网络层错误分类：超时/中断类归 Timeout，其余 Unknown。
func TestClassifyNetworkErr(t *testing.T) {
	if got := ClassifyNetworkErr(nil); got != ClassUnknown {
		t.Fatalf("nil 应为 unknown: %s", got)
	}
	for _, msg := range []string{
		"dial tcp 127.0.0.1:1: connect: connection refused",
		"read: connection reset by peer",
		"unexpected EOF",
		"context deadline exceeded",
	} {
		if got := ClassifyNetworkErr(errors.New(msg)); got != ClassTimeout {
			t.Fatalf("%q 应为 timeout，实际 %s", msg, got)
		}
	}
	if got := ClassifyNetworkErr(fmt.Errorf("weird failure")); got != ClassUnknown {
		t.Fatalf("普通错误应为 unknown: %s", got)
	}
}
