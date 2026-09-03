// log_mine_test.go GET /api/log/mine 会话作用域与白名单测试（M3-wave4）：
// 登录态可访问、强制当前用户作用域（user_id 查询参数被忽略，不可越权枚举
// 他人日志）、响应为 logMineView 白名单字段（不含 channel_id/user_id）、
// 过滤参数生效、未登录 401、普通用户访问管理列表 /api/log 403、admin 管理
// 列表行为不变（root 经 /api/log 可查全部用户日志）。
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

// seedConsumeLog 写入一条指定用户/模型的计费日志并返回
// （命名避开 user_stats_test.go 既有 seedLog）。
func seedConsumeLog(t *testing.T, st *store.Store, userID int64, modelName string) model.Log {
	t.Helper()
	l := model.Log{
		UserID:           userID,
		TokenID:          42,
		ChannelID:        7,
		Protocol:         "openai",
		ModelName:        modelName,
		PromptTokens:     100,
		CompletionTokens: 50,
		Quota:            123,
		UseTime:          2,
		CreatedTime:      time.Now().Unix(),
	}
	if err := st.Write.Create(&l).Error; err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
	return l
}

// TestLogMineOwnershipScope 个人视角：作用域 + 白名单 + 越权拒绝 + 管理面隔离。
func TestLogMineOwnershipScope(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	alice := seedUser(t, st, "alice", "pw-alice", model.RoleUser)
	bob := seedUser(t, st, "bob", "pw-bob", model.RoleUser)
	seedConsumeLog(t, st, alice.ID, "gpt-x")
	seedConsumeLog(t, st, alice.ID, "claude-y")
	seedConsumeLog(t, st, bob.ID, "gpt-x")

	// alice 登录取得普通用户会话。
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": "alice", "password": "pw-alice",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("alice 登录应 200，实际 %d", w.Code)
	}
	aliceCookie := sessionCookieFrom(t, w)

	// 1) 登录态可访问：仅含自己的日志（id 降序）。
	w = doJSON(t, r, http.MethodGet, "/api/log/mine", aliceCookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mine 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Data struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, w.Body.String())
	}
	if list.Data.Total != 2 || len(list.Data.Items) != 2 {
		t.Fatalf("alice 应仅见自己的 2 条日志，实际 total=%d len=%d body=%s",
			list.Data.Total, len(list.Data.Items), w.Body.String())
	}
	for _, item := range list.Data.Items {
		if _, ok := item["user_id"]; ok {
			t.Fatalf("mine 响应白名单不含 user_id: %v", item)
		}
		if _, ok := item["channel_id"]; ok {
			t.Fatalf("mine 响应白名单不含 channel_id: %v", item)
		}
	}

	// 2) 字段白名单：channel_id/user_id 键名不得出现在响应文本中。
	for _, banned := range []string{`"channel_id"`, `"user_id"`} {
		if strings.Contains(w.Body.String(), banned) {
			t.Fatalf("mine 响应不得含 %s: %s", banned, w.Body.String())
		}
	}

	// 3) 越权探测：user_id 指向他人也被忽略，恒当前用户作用域。
	w = doJSON(t, r, http.MethodGet, "/api/log/mine?user_id="+itoa(bob.ID), aliceCookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":2`) {
		t.Fatalf("user_id 参数应被忽略（恒当前用户作用域），实际 %d %s", w.Code, w.Body.String())
	}

	// 4) 过滤参数：model_name 精确过滤。
	w = doJSON(t, r, http.MethodGet, "/api/log/mine?model_name=claude-y", aliceCookie, nil)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("model_name 过滤应命中 1 条，实际 %s", w.Body.String())
	}

	// 5) 未登录 401；普通用户访问管理面 /api/log 403。
	if w := doJSON(t, r, http.MethodGet, "/api/log/mine", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/log", aliceCookie, nil); w.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问管理列表应 403，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 6) admin 管理列表行为不变：root 经 /api/log 可查全部用户（3 条）日志。
	w = doJSON(t, r, http.MethodGet, "/api/log", cookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":3`) {
		t.Fatalf("root 管理列表应查到全部 3 条，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"channel_id":7`) {
		t.Fatalf("管理列表应含渠道字段（行为不变）: %s", w.Body.String())
	}

	// 7) root 个人视角同样受会话作用域约束（root 自己无日志 → 空列表）。
	w = doJSON(t, r, http.MethodGet, "/api/log/mine", cookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("root 个人视角应仅见本人（0 条），实际 %d %s", w.Code, w.Body.String())
	}
}
