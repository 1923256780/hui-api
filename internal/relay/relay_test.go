package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newTestGinContext 构造绑定自定义 ResponseWriter 的 gin 测试上下文。
func newTestGinContext(w http.ResponseWriter) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	return c
}

// flushCounter 包装 Recorder 统计 Flush 次数（验证逐事件 flush 语义）。
type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCounter) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

// TestJoinBaseURL base_url 拼接规则：尾斜杠归一与 /v1 去重。
func TestJoinBaseURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://a.com", "/v1/chat/completions", "https://a.com/v1/chat/completions"},
		{"https://a.com/", "/v1/chat/completions", "https://a.com/v1/chat/completions"},
		{"https://a.com/v1", "/v1/chat/completions", "https://a.com/v1/chat/completions"},
		{"https://a.com/v1/", "/v1/chat/completions", "https://a.com/v1/chat/completions"},
		{"http://127.0.0.1:9000", "/v1/messages/count_tokens", "http://127.0.0.1:9000/v1/messages/count_tokens"},
		{"", "/v1/messages", "/v1/messages"},
	}
	for _, tc := range cases {
		if got := JoinBaseURL(tc.base, tc.path); got != tc.want {
			t.Errorf("JoinBaseURL(%q,%q)=%q，期望 %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// TestNewUpstreamRequestSchemeCheck scheme 白名单：仅 http/https。
func TestNewUpstreamRequestSchemeCheck(t *testing.T) {
	if _, err := NewUpstreamRequest(http.MethodPost, "https://a.com/v1/x", nil); err != nil {
		t.Fatalf("https 不应报错: %v", err)
	}
	if _, err := NewUpstreamRequest(http.MethodPost, "http://a.com/v1/x", nil); err != nil {
		t.Fatalf("http 不应报错: %v", err)
	}
	for _, bad := range []string{"ftp://a.com/x", "file:///etc/passwd", "::bad::"} {
		if _, err := NewUpstreamRequest(http.MethodPost, bad, nil); err == nil {
			t.Errorf("非法地址 %q 应报错", bad)
		}
	}
}

// TestForwardStream 验证 SSE 逐事件转发：原文不改写、事件边界 Flush、data 回调。
func TestForwardStream(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"a":1}`,
		``,
		`data: {"b":2}`,
		`: keep-alive comment`,
		``,
		`data: [DONE]`,
		``,
	}, "\n") + "\n" // 真实 SSE 流每行都以换行结尾

	rec := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	var events []string
	total, err := ForwardStream(newTestGinContext(rec), strings.NewReader(upstream), func(data []byte) {
		events = append(events, string(data))
	})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}
	if total <= 0 {
		t.Fatalf("转发字节数应 >0，实际 %d", total)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"a":1}`) || !strings.Contains(body, `data: {"b":2}`) || !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("转发内容应保留事件原文: %q", body)
	}
	if strings.Count(body, "\n") < 7 {
		t.Fatalf("换行结构应保留: %q", body)
	}
	if rec.flushes < 3 {
		t.Fatalf("每个事件边界应 Flush 一次（>=3），实际 %d", rec.flushes)
	}
	if len(events) != 3 {
		t.Fatalf("data 回调应触发 3 次，实际 %d: %v", len(events), events)
	}
	if events[2] != "[DONE]" {
		t.Fatalf("末事件应为 [DONE]，实际 %q", events[2])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}
}

// TestForwardStreamClientDisconnect 客户端写失败时静默结束（返回 nil 错误）。
func TestForwardStreamClientDisconnect(t *testing.T) {
	// 用错误 writer 验证写失败路径（客户端断开等价场景）。
	_, err := ForwardStream(newTestGinContext(failWriter{}), strings.NewReader("data: x\n\n"), nil)
	if err != nil {
		t.Fatalf("写失败应静默结束，实际 %v", err)
	}
}

// failWriter 是始终写失败的 ResponseWriter。
type failWriter struct{}

func (failWriter) Header() http.Header       { return http.Header{} }
func (failWriter) Write([]byte) (int, error) { return 0, errWriteFailed }
func (failWriter) WriteHeader(int)           {}
func (failWriter) Flush()                    {}

var errWriteFailed = errNew("write failed")

func errNew(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }
