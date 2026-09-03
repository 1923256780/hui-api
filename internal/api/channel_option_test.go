package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// loginRoot 引导 root 并登录，返回会话 Cookie 头值（"session=..."）。
func loginRoot(t *testing.T, r *gin.Engine, st *store.Store) string {
	t.Helper()
	if _, err := EnsureRootUser(st); err != nil {
		t.Fatalf("引导 root 失败: %v", err)
	}
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "",
		map[string]string{"username": RootUsername, "password": DefaultRootPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("root 登录应 200，实际 %d", w.Code)
	}
	return sessionCookieFrom(t, w)
}

// itoa 生成路径用 ID 字符串。
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// mustDataID 从创建响应 {"data":{"id":N,...}} 提取 id。
func mustDataID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var out struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Data.ID == 0 {
		t.Fatalf("解析 data.id 失败: %v body=%s", err, w.Body.String())
	}
	return out.Data.ID
}

// TestChannelCRUDIdempotentPut 渠道 CRUD 与整对象幂等写：
// 密钥脱敏回显；PUT 全量覆盖（显式传入字段不丢）；key 空=保留旧值；
// 同内容两次 PUT 结果一致；删除后 404。
func TestChannelCRUDIdempotentPut(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	// 未登录 → 401。
	if w := doJSON(t, r, http.MethodGet, "/api/channel", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}

	// 创建（201）。
	createBody := map[string]any{
		"name": "up1", "type": model.ChannelTypeOpenAICompatible,
		"base_url": "https://up.example", "key": "sk-live-secret-001",
		"models": "m1,m2", "priority": 10, "weight": 5, "status": 1,
	}
	w := doJSON(t, r, http.MethodPost, "/api/channel", cookie, createBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sk-***") {
		t.Fatalf("key 应脱敏回显: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-live-secret-001") {
		t.Fatal("创建响应不得泄露明文密钥")
	}
	id := mustDataID(t, w)

	// 列表与单个：同样脱敏。
	w = doJSON(t, r, http.MethodGet, "/api/channel?page=1&page_size=10", cookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("列表应含 1 条，实际 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-live-secret-001") {
		t.Fatal("列表不得泄露明文密钥")
	}
	w = doJSON(t, r, http.MethodGet, "/api/channel/"+itoa(id), cookie, nil)
	if !strings.Contains(w.Body.String(), `"priority":10`) {
		t.Fatalf("单个应返回 priority=10: %s", w.Body.String())
	}

	// PUT 整对象替换：priority 显式改为 1，weight 显式传 5（不丢字段），key 留空=保留。
	putBody := map[string]any{
		"name": "up1", "type": model.ChannelTypeOpenAICompatible,
		"base_url": "https://up.example", "key": "",
		"models": "m1,m2", "priority": 1, "weight": 5, "status": 1,
	}
	w = doJSON(t, r, http.MethodPut, "/api/channel/"+itoa(id), cookie, putBody)
	if w.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"priority":10`) || !strings.Contains(w.Body.String(), `"priority":1`) {
		t.Fatalf("priority 应为 1: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"weight":5`) {
		t.Fatalf("显式传入的 weight 不得丢失: %s", w.Body.String())
	}

	// 幂等：同一 body 再 PUT，结果一致。
	w2 := doJSON(t, r, http.MethodPut, "/api/channel/"+itoa(id), cookie, putBody)
	if w.Body.String() != w2.Body.String() {
		t.Fatalf("同内容 PUT 应幂等:\n first=%s\nsecond=%s", w.Body.String(), w2.Body.String())
	}

	// 密钥仍在库内（未因空 key 清空）。
	var ch model.Channel
	if err := st.Read.First(&ch, id).Error; err != nil || ch.Key != "sk-live-secret-001" {
		t.Fatalf("空 key PUT 应保留旧密钥: %v key=%q", err, ch.Key)
	}

	// 删除后 404。
	if w := doJSON(t, r, http.MethodDelete, "/api/channel/"+itoa(id), cookie, nil); w.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/channel/"+itoa(id), cookie, nil); w.Code != http.StatusNotFound {
		t.Fatalf("删除后应 404，实际 %d", w.Code)
	}
}

// TestChannelTestEndpoint 连通性测试：可达上游返回 success=true 与耗时；
// 不可达上游返回 success=false（HTTP 仍 200，测试结果语义）。
func TestChannelTestEndpoint(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/models" {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer up.Close()

	// 可达上游。
	w := doJSON(t, r, http.MethodPost, "/api/channel", cookie, map[string]any{
		"name": "ok", "type": model.ChannelTypeOpenAICompatible,
		"base_url": up.URL, "key": "sk-k", "models": "m1",
	})
	id := mustDataID(t, w)
	w = doJSON(t, r, http.MethodPost, "/api/channel/test/"+itoa(id), cookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("可达上游应 success=true: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status_code":200`) || !strings.Contains(w.Body.String(), `"time_ms":`) {
		t.Fatalf("应返回状态码与耗时: %s", w.Body.String())
	}

	// 不可达上游（端口 1 立即拒绝）。
	w = doJSON(t, r, http.MethodPost, "/api/channel", cookie, map[string]any{
		"name": "dead", "type": model.ChannelTypeOpenAICompatible,
		"base_url": "http://127.0.0.1:1", "key": "sk-k", "models": "m1",
	})
	deadID := mustDataID(t, w)
	start := time.Now()
	w = doJSON(t, r, http.MethodPost, "/api/channel/test/"+itoa(deadID), cookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"success":false`) {
		t.Fatalf("不可达上游应 success=false: %d %s", w.Code, w.Body.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("不可达上游应快速失败（端口拒绝），不应等待满超时")
	}
}

// TestOptionHotReload options 批量写与热更：白名单校验（拒 schema_version 与
// 未收录键）、值长度上限、写后 Runtime 版本递增且新值即时可读。
func TestOptionHotReload(t *testing.T) {
	r, st, h := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	before := h.rt.Version()

	// 拒绝内部键 schema_version。
	w := doJSON(t, r, http.MethodPut, "/api/option", cookie,
		map[string]any{"options": map[string]string{"schema_version": "99"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("schema_version 应被拒绝，实际 %d", w.Code)
	}

	// 拒绝未收录键。
	w = doJSON(t, r, http.MethodPut, "/api/option", cookie,
		map[string]any{"options": map[string]string{"SomeUnknownKey": "x"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未收录键应被拒绝，实际 %d", w.Code)
	}

	// 值超长。
	long := make([]byte, optionValueMaxLen+1)
	for i := range long {
		long[i] = 'a'
	}
	w = doJSON(t, r, http.MethodPut, "/api/option", cookie,
		map[string]any{"options": map[string]string{"GroupRatio": string(long)}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长值应被拒绝，实际 %d", w.Code)
	}

	// 合法批量写：分组倍率 + 限流开关。
	w = doJSON(t, r, http.MethodPut, "/api/option", cookie,
		map[string]any{"options": map[string]string{
			"GroupRatio":                   `{"vip":2.0}`,
			"ModelRequestRateLimitEnabled": "true",
		}})
	if w.Code != http.StatusOK {
		t.Fatalf("合法批量写应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if h.rt.Version() <= before {
		t.Fatalf("写后应触发 Reload 版本递增：before=%d after=%d", before, h.rt.Version())
	}
	if v, _ := h.rt.Get("GroupRatio"); v != `{"vip":2.0}` {
		t.Fatalf("热更后 GroupRatio 应可读，实际 %q", v)
	}
	if !h.rt.GetBool("ModelRequestRateLimitEnabled", false) {
		t.Fatal("热更后限流开关应为 true")
	}

	// GET 全量包含新键。
	if w := doJSON(t, r, http.MethodGet, "/api/option", cookie, nil); !strings.Contains(w.Body.String(), "GroupRatio") {
		t.Fatalf("GET 应包含新写入键: %s", w.Body.String())
	}
}
