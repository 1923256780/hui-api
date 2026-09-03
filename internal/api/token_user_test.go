// token_user_test.go 令牌/用户/兑换码/日志端点测试（M2-wave1）：
// CRUD 与整对象幂等写、明文 key 一次性、写后失效鉴权缓存（与 gateway 联动）、
// 改密失效旧会话（auth_version 端到端）、防自锁与级联删除、批量生成、过滤查询。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
)

// mustDataString 从响应 {"data":{FIELD:"..."}} 提取字符串字段。
func mustDataString(t *testing.T, w *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 data 失败: %v body=%s", err, w.Body.String())
	}
	v, _ := out.Data[field].(string)
	return v
}

// mustTokenID 从令牌创建响应 {"data":{"token":{"id":N,...},"key":...}} 提取 id。
func mustTokenID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var out struct {
		Data struct {
			Token struct {
				ID int64 `json:"id"`
			} `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Data.Token.ID == 0 {
		t.Fatalf("解析 data.token.id 失败: %v body=%s", err, w.Body.String())
	}
	return out.Data.Token.ID
}

// TestTokenCRUDAndKeyOnce 令牌 CRUD：明文 key 仅创建响应返回一次；
// 列表/更新响应不泄露 key/key_hash；PUT 整对象幂等；group 空 → default。
func TestTokenCRUDAndKeyOnce(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	u := seedUser(t, st, "alice", "pw-alice", model.RoleUser)

	w := doJSON(t, r, http.MethodPost, "/api/token", cookie, map[string]any{
		"user_id": u.ID, "name": "t1", "quota": 500000,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("创建令牌应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	plain := mustDataString(t, w, "key")
	if !strings.HasPrefix(plain, "sk-") {
		t.Fatalf("明文 key 应 sk- 前缀: %q", plain)
	}
	if strings.Contains(w.Body.String(), "key_hash") || strings.Contains(w.Body.String(), `"key_hash"`) {
		t.Fatal("创建响应不得泄露 key_hash")
	}
	id := mustTokenID(t, w)

	// remain 缺省 = quota。
	if !strings.Contains(w.Body.String(), `"remain_quota":500000`) {
		t.Fatalf("remain 缺省应等于 quota: %s", w.Body.String())
	}

	// 列表不泄露明文与哈希。
	w = doJSON(t, r, http.MethodGet, "/api/token", cookie, nil)
	if strings.Contains(w.Body.String(), plain) || strings.Contains(w.Body.String(), "key_hash") {
		t.Fatalf("列表不得泄露 key/key_hash: %s", w.Body.String())
	}

	// PUT 整对象替换：显式字段全量生效，同 body 幂等。
	putBody := map[string]any{
		"user_id": u.ID, "name": "t1b", "status": 1, "quota": 800000,
		"remain_quota": 700000, "group": "vip", "tpm_rpm": `{"tpm":1000,"rpm":10}`,
		"tags": "beta", "model_limits": "m1,m2", "allow_ips": "10.0.0.0/8",
	}
	w = doJSON(t, r, http.MethodPut, "/api/token/"+itoa(id), cookie, putBody)
	if w.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"group":"vip"`) ||
		!strings.Contains(w.Body.String(), `"name":"t1b"`) ||
		!strings.Contains(w.Body.String(), `"remain_quota":700000`) {
		t.Fatalf("PUT 字段应全量生效: %s", w.Body.String())
	}
	w2 := doJSON(t, r, http.MethodPut, "/api/token/"+itoa(id), cookie, putBody)
	if w.Body.String() != w2.Body.String() {
		t.Fatalf("同内容 PUT 应幂等:\nfirst=%s\nsecond=%s", w.Body.String(), w2.Body.String())
	}

	// group 置空 → 缺省 default。
	empty := map[string]any{"user_id": u.ID, "name": "t1c", "status": 1, "quota": 1, "group": ""}
	w = doJSON(t, r, http.MethodPut, "/api/token/"+itoa(id), cookie, empty)
	if !strings.Contains(w.Body.String(), `"group":"default"`) {
		t.Fatalf("group 空应缺省 default: %s", w.Body.String())
	}

	// 删除后列表为空。
	if w := doJSON(t, r, http.MethodDelete, "/api/token/"+itoa(id), cookie, nil); w.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/token", cookie, nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("删除后列表应为空: %s", w.Body.String())
	}
}

// TestTokenWriteInvalidatesAuthCache 令牌增删改后失效鉴权缓存（与 gateway 联动）：
// 先 Authenticate 填缓存，再经 API 禁用/删除，断言缓存未命中旧值。
// （缓存命中路径仍走 validateToken 校验快照，若挂接失效会误放行 enabled 快照。）
func TestTokenWriteInvalidatesAuthCache(t *testing.T) {
	r, st, h := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	u := seedUser(t, st, "bob", "pw-bob", model.RoleUser)
	w := doJSON(t, r, http.MethodPost, "/api/token", cookie, map[string]any{
		"user_id": u.ID, "name": "t2", "quota": 100000,
	})
	plain := mustDataString(t, w, "key")
	id := mustTokenID(t, w)

	// 与管理面同一 TokenAuth 实例（写后失效挂接的目标；独立实例缓存隔离验证无效）。
	auth := h.gw.Auth()
	if _, err := auth.Authenticate(plain); err != nil {
		t.Fatalf("初始鉴权应通过: %v", err)
	}

	// 禁用 → 鉴权失败（缓存应被失效）。
	w = doJSON(t, r, http.MethodPut, "/api/token/"+itoa(id), cookie, map[string]any{
		"user_id": u.ID, "name": "t2", "status": model.StatusDisabled, "quota": 100000,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("禁用应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if _, err := auth.Authenticate(plain); err == nil {
		t.Fatal("禁用后鉴权应失败（写后失效挂接缺失或未生效）")
	}

	// 重新启用 → 鉴权恢复。
	w = doJSON(t, r, http.MethodPut, "/api/token/"+itoa(id), cookie, map[string]any{
		"user_id": u.ID, "name": "t2", "status": model.StatusEnabled, "quota": 100000,
	})
	if _, err := auth.Authenticate(plain); err != nil {
		t.Fatalf("重新启用后鉴权应通过: %v", err)
	}

	// 删除 → 鉴权失败。
	if w := doJSON(t, r, http.MethodDelete, "/api/token/"+itoa(id), cookie, nil); w.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", w.Code)
	}
	if _, err := auth.Authenticate(plain); err == nil {
		t.Fatal("删除后鉴权应失败")
	}
}

// TestUserCRUDPasswordChangeInvalidates 用户 CRUD 与改密失效旧会话（端到端）：
// 创建（用户名重复 409）→ 登录 → root 改密 → 旧会话 401、旧密码拒绝、新密码通过。
func TestUserCRUDPasswordChangeInvalidates(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	w := doJSON(t, r, http.MethodPost, "/api/user", cookie, map[string]any{
		"username": "alice", "password": "pw-old", "quota": 1000, "group": "vip",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("创建用户应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	aliceID := mustDataID(t, w)

	w = doJSON(t, r, http.MethodPost, "/api/user", cookie, map[string]any{
		"username": "alice", "password": "x",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("重复用户名应 409，实际 %d", w.Code)
	}

	// alice 登录（公开端点），取得旧会话。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": "alice", "password": "pw-old",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("alice 登录应 200，实际 %d", w.Code)
	}
	aliceCookie := sessionCookieFrom(t, w)

	// root 改 alice 口令（整对象写；role 缺省归一化普通用户）。
	w = doJSON(t, r, http.MethodPut, "/api/user/"+itoa(aliceID), cookie, map[string]any{
		"username": "alice", "password": "pw-new", "status": model.StatusEnabled,
		"quota": 1000, "group": "vip",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("改密应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 旧会话失效：受保护端点 401。
	w = doJSON(t, r, http.MethodGet, "/api/user", aliceCookie, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("改密后旧会话应 401，实际 %d", w.Code)
	}

	// 旧密码拒绝、新密码通过。
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": "alice", "password": "pw-old",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应 401，实际 %d", w.Code)
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": "alice", "password": "pw-new",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("新密码应 200，实际 %d", w.Code)
	}
}

// TestSelfLockoutAndDeleteAdminForbidden 防自锁：root 不可自改 role/status，
// 不可删除管理员账号。
func TestSelfLockoutAndDeleteAdminForbidden(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	var root model.User
	if err := st.Read.Where("username = ?", RootUsername).First(&root).Error; err != nil {
		t.Fatalf("查询 root 失败: %v", err)
	}

	// 自改 role → 400 self_lockout。
	w := doJSON(t, r, http.MethodPut, "/api/user/"+itoa(root.ID), cookie, map[string]any{
		"username": RootUsername, "role": model.RoleUser, "status": model.StatusEnabled,
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "self_lockout") {
		t.Fatalf("root 自改 role 应 self_lockout 400，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 自改 status → 同样拒绝。
	w = doJSON(t, r, http.MethodPut, "/api/user/"+itoa(root.ID), cookie, map[string]any{
		"username": RootUsername, "role": model.RoleAdmin, "status": model.StatusDisabled,
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "self_lockout") {
		t.Fatalf("root 自改 status 应 self_lockout 400，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 删除管理员 → 400。
	w = doJSON(t, r, http.MethodDelete, "/api/user/"+itoa(root.ID), cookie, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "delete_admin_forbidden") {
		t.Fatalf("删除管理员应 400，实际 %d body=%s", w.Code, w.Body.String())
	}

	// root 显式传同 role/status 改自己的展示名 → 允许（不触发自锁）。
	w = doJSON(t, r, http.MethodPut, "/api/user/"+itoa(root.ID), cookie, map[string]any{
		"username": RootUsername, "role": model.RoleAdmin,
		"status": model.StatusEnabled, "display_name": "boss",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("root 改自身资料（role/status 显式不变）应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestDeleteUserCascadesTokens 删除用户级联删除其令牌并失效鉴权缓存。
func TestDeleteUserCascadesTokens(t *testing.T) {
	r, st, h := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	w := doJSON(t, r, http.MethodPost, "/api/user", cookie, map[string]any{
		"username": "bob", "password": "pw-bob",
	})
	bobID := mustDataID(t, w)

	w = doJSON(t, r, http.MethodPost, "/api/token", cookie, map[string]any{
		"user_id": bobID, "name": "bt", "quota": 1000,
	})
	plain := mustDataString(t, w, "key")

	// 与管理面同一 TokenAuth 实例：验证级联删除触发的缓存失效。
	auth := h.gw.Auth()
	if _, err := auth.Authenticate(plain); err != nil {
		t.Fatalf("鉴权应通过: %v", err)
	}

	w = doJSON(t, r, http.MethodDelete, "/api/user/"+itoa(bobID), cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("删除用户应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if _, err := auth.Authenticate(plain); err == nil {
		t.Fatal("级联删除后令牌鉴权应失败")
	}
	if w := doJSON(t, r, http.MethodGet, "/api/token?user_id="+itoa(bobID), cookie, nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("令牌应被级联删除: %s", w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": "bob", "password": "pw-bob",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("已删用户登录应 401，实际 %d", w.Code)
	}
}

// TestRedemptionBatchGenerate 兑换码批量生成：key 前缀/数量/参数校验/列表/删除。
func TestRedemptionBatchGenerate(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	w := doJSON(t, r, http.MethodPost, "/api/redemption", cookie, map[string]any{
		"count": 3, "name": "gift", "quota": 500000,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("生成应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Data.Keys) != 3 {
		t.Fatalf("应返回 3 个 key: %s", w.Body.String())
	}
	seen := map[string]bool{}
	for _, k := range out.Data.Keys {
		if !strings.HasPrefix(k, "redd-") {
			t.Fatalf("key 应 redd- 前缀: %q", k)
		}
		if seen[k] {
			t.Fatalf("key 应唯一: %q", k)
		}
		seen[k] = true
	}

	// 参数校验。
	if w := doJSON(t, r, http.MethodPost, "/api/redemption", cookie, map[string]any{"count": 0, "quota": 1}); w.Code != http.StatusBadRequest {
		t.Fatalf("count=0 应 400，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/redemption", cookie, map[string]any{"count": 101, "quota": 1}); w.Code != http.StatusBadRequest {
		t.Fatalf("count=101 应 400，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/redemption", cookie, map[string]any{"count": 1, "quota": 0}); w.Code != http.StatusBadRequest {
		t.Fatalf("quota=0 应 400，实际 %d", w.Code)
	}

	// 列表 3 条，删除后 2 条。
	w = doJSON(t, r, http.MethodGet, "/api/redemption", cookie, nil)
	if !strings.Contains(w.Body.String(), `"total":3`) {
		t.Fatalf("列表应 3 条: %s", w.Body.String())
	}
	var list struct {
		Data struct {
			Items []model.Redemption `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data.Items) != 3 {
		t.Fatalf("解析列表失败: %s", w.Body.String())
	}
	delID := list.Data.Items[0].ID
	if w := doJSON(t, r, http.MethodDelete, "/api/redemption/"+itoa(delID), cookie, nil); w.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodGet, "/api/redemption", cookie, nil); !strings.Contains(w.Body.String(), `"total":2`) {
		t.Fatalf("删除后应 2 条: %s", w.Body.String())
	}
	if w := doJSON(t, r, http.MethodDelete, "/api/redemption/"+itoa(delID), cookie, nil); w.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实际 %d", w.Code)
	}
}

// TestLogFilters 日志分页查询与过滤：全量/user_id/model_name/channel_id/时间区间/
// 最新在前排序。
func TestLogFilters(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	now := time.Now().Unix()
	logs := []model.Log{
		{UserID: 1, TokenID: 11, ChannelID: 21, ModelName: "m1", Quota: 10, CreatedTime: now - 100},
		{UserID: 2, TokenID: 12, ChannelID: 22, ModelName: "m2", Quota: 20, CreatedTime: now - 50},
		{UserID: 1, TokenID: 13, ChannelID: 21, ModelName: "m2", Quota: 30, CreatedTime: now},
	}
	for i := range logs {
		if err := st.Write.Create(&logs[i]).Error; err != nil {
			t.Fatalf("造日志失败: %v", err)
		}
	}

	assertTotal := func(path, want string) {
		t.Helper()
		w := doJSON(t, r, http.MethodGet, path, cookie, nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":`+want) {
			t.Fatalf("%s 应 total=%s，实际 %d %s", path, want, w.Code, w.Body.String())
		}
	}
	assertTotal("/api/log", "3")
	assertTotal("/api/log?user_id=1", "2")
	assertTotal("/api/log?user_id=2", "1")
	assertTotal("/api/log?model_name=m2", "2")
	assertTotal("/api/log?channel_id=21", "2")
	assertTotal("/api/log?token_id=13", "1")
	assertTotal("/api/log?start_timestamp="+itoa(now-60)+"&end_timestamp="+itoa(now), "2")

	// 排序：id desc，最新在前。
	var list struct {
		Data struct {
			Items []model.Log `json:"items"`
		} `json:"data"`
	}
	w := doJSON(t, r, http.MethodGet, "/api/log", cookie, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data.Items) != 3 {
		t.Fatalf("解析列表失败: %s", w.Body.String())
	}
	if list.Data.Items[0].ID != logs[2].ID {
		t.Fatalf("最新日志应在前: want id=%d got id=%d", logs[2].ID, list.Data.Items[0].ID)
	}
}
