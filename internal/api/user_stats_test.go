// user_stats_test.go GET /api/user/stats 自服务统计端点测试（M2 收官 Task #19）：
// 登录态 200 且仅聚合当前用户「今日」日志（昨日日志不计入）；user_id 查询
// 参数被忽略（恒会话用户作用域）；他人数据零泄漏（他人模型名不得出现在
// 响应，汇总数值逐位断言）；无日志用户返回全零空态而非错误；未登录 401；
// 普通用户访问管理面 GET /api/log 回归 403（看板数据源已切换）。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// seedLog 直插一条计费日志（测试用；真实写入路径在转发面 hook 异步旁路）。
func seedLog(t *testing.T, st *store.Store, l *model.Log) {
	t.Helper()
	if err := st.Write.Create(l).Error; err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
}

// userStatsPayload 是 /api/user/stats 的 data 载荷形状。
type userStatsPayload struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Tokens           int64 `json:"tokens"`
	Quota            int64 `json:"quota"`
	Models           []struct {
		ModelName        string `json:"model_name"`
		Requests         int64  `json:"requests"`
		PromptTokens     int64  `json:"prompt_tokens"`
		CompletionTokens int64  `json:"completion_tokens"`
		Quota            int64  `json:"quota"`
	} `json:"models"`
}

// TestUserStatsScopeAndLeakage 统计作用域、今日口径、越权拒绝与零泄漏。
func TestUserStatsScopeAndLeakage(t *testing.T) {
	r, st, _ := newTestAPI(t)

	alice := seedUser(t, st, "alice", "pw-alice", model.RoleUser)
	bob := seedUser(t, st, "bob", "pw-bob", model.RoleUser)

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, now.Location()).Unix()
	yesterday := today - 86400

	// alice：今日两条（model-b 消耗更高）+ 昨日一条（不计入今日）。
	seedLog(t, st, &model.Log{UserID: alice.ID, ModelName: "model-a",
		PromptTokens: 100, CompletionTokens: 50, Quota: 300, CreatedTime: today})
	seedLog(t, st, &model.Log{UserID: alice.ID, ModelName: "model-b",
		PromptTokens: 10, CompletionTokens: 5, Quota: 700, CreatedTime: today})
	seedLog(t, st, &model.Log{UserID: alice.ID, ModelName: "model-a",
		PromptTokens: 999, CompletionTokens: 999, Quota: 999999, CreatedTime: yesterday})
	// bob：今日一条，alice 必须完全看不到。
	seedLog(t, st, &model.Log{UserID: bob.ID, ModelName: "bob-secret-model",
		PromptTokens: 12345, CompletionTokens: 6789, Quota: 424242, CreatedTime: today})

	// alice 登录取得普通用户会话。
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "alice", "password": "pw-alice"})
	if w.Code != http.StatusOK {
		t.Fatalf("alice 登录应 200，实际 %d", w.Code)
	}
	aliceCookie := sessionCookieFrom(t, w)

	// 1) 登录态 200：仅聚合今日两条，汇总与分布逐位断言。
	w = doJSON(t, r, http.MethodGet, "/api/user/stats", aliceCookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Data userStatsPayload `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, w.Body.String())
	}
	s := got.Data
	if s.Requests != 2 {
		t.Fatalf("应仅统计今日 2 条，实际 requests=%d body=%s", s.Requests, w.Body.String())
	}
	if s.PromptTokens != 110 || s.CompletionTokens != 55 || s.Tokens != 165 {
		t.Fatalf("tokens 汇总不符: prompt=%d completion=%d tokens=%d", s.PromptTokens, s.CompletionTokens, s.Tokens)
	}
	if s.Quota != 1000 {
		t.Fatalf("quota 应为 300+700=1000（昨日 999999 不计入），实际 %d", s.Quota)
	}
	if len(s.Models) != 2 {
		t.Fatalf("模型分布应有 2 行，实际 %d body=%s", len(s.Models), w.Body.String())
	}
	if s.Models[0].ModelName != "model-b" || s.Models[0].Quota != 700 || s.Models[0].Requests != 1 {
		t.Fatalf("分布首行应为 model-b（quota 降序），实际 %+v", s.Models[0])
	}
	if s.Models[1].ModelName != "model-a" || s.Models[1].Quota != 300 ||
		s.Models[1].PromptTokens != 100 || s.Models[1].CompletionTokens != 50 {
		t.Fatalf("分布次行应为 model-a 今日数据，实际 %+v", s.Models[1])
	}

	// 2) 他人数据零泄漏：bob 的模型名不得出现在响应体（数值经逐位断言兜底）。
	if strings.Contains(w.Body.String(), "bob-secret-model") {
		t.Fatalf("响应不得含他人模型名: %s", w.Body.String())
	}

	// 3) 越权探测：user_id 指向他人被忽略，恒当前用户作用域。
	w = doJSON(t, r, http.MethodGet, "/api/user/stats?user_id="+itoa(bob.ID), aliceCookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"requests":2`) {
		t.Fatalf("user_id 参数应被忽略（恒会话用户作用域），实际 %d %s", w.Code, w.Body.String())
	}

	// 4) 空态：无日志用户 → 全零 + 空分布（200，非错误）。
	seedUser(t, st, "carol", "pw-carol", model.RoleUser)
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": "carol", "password": "pw-carol"})
	if w.Code != http.StatusOK {
		t.Fatalf("carol 登录应 200，实际 %d", w.Code)
	}
	carolCookie := sessionCookieFrom(t, w)
	w = doJSON(t, r, http.MethodGet, "/api/user/stats", carolCookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("无日志用户应 200 空态，实际 %d body=%s", w.Code, w.Body.String())
	}
	var empty struct {
		Data userStatsPayload `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatalf("解析空态响应失败: %v", err)
	}
	if empty.Data.Requests != 0 || empty.Data.Quota != 0 || empty.Data.Tokens != 0 || len(empty.Data.Models) != 0 {
		t.Fatalf("空态应全零，实际 %s", w.Body.String())
	}

	// 5) 未登录 401。
	if w := doJSON(t, r, http.MethodGet, "/api/user/stats", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}

	// 6) 回归：普通用户访问管理面 GET /api/log 仍 403（看板已切换数据源）。
	if w := doJSON(t, r, http.MethodGet, "/api/log", aliceCookie, nil); w.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问管理面日志应 403，实际 %d body=%s", w.Code, w.Body.String())
	}
}
