// token_mine_test.go GET /api/token/mine 所有权作用域测试（M2 浏览器验收缺陷修复）：
// 登录态可访问、强制当前用户作用域（user_id 查询参数被忽略，不可越权枚举他人
// 令牌）、响应为白名单字段（不泄露密钥材料与管理配置字段）、未登录 401、
// 普通用户访问管理列表 GET /api/token 403。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/1923256780/hui-api/internal/model"
)

// TestTokenMineOwnershipScope 名下令牌列表：所有权作用域 + 字段白名单 + 越权拒绝。
func TestTokenMineOwnershipScope(t *testing.T) {
	r, st, _ := newTestAPI(t)
	cookie := loginRoot(t, r, st)

	alice := seedUser(t, st, "alice", "pw-alice", model.RoleUser)
	bob := seedUser(t, st, "bob", "pw-bob", model.RoleUser)
	for i := 0; i < 2; i++ {
		w := doJSON(t, r, http.MethodPost, "/api/token", cookie, map[string]any{
			"user_id": alice.ID, "name": "alice-t", "quota": 500000,
			"tpm_rpm": `{"tpm":1000,"rpm":10}`, "tags": `["team-a"]`, "allow_ips": "10.0.0.0/8",
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("创建 alice 令牌应 201，实际 %d body=%s", w.Code, w.Body.String())
		}
	}
	if w := doJSON(t, r, http.MethodPost, "/api/token", cookie, map[string]any{
		"user_id": bob.ID, "name": "bob-t", "quota": 1,
	}); w.Code != http.StatusCreated {
		t.Fatalf("创建 bob 令牌应 201，实际 %d body=%s", w.Code, w.Body.String())
	}

	// alice 登录取得普通用户会话。
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": "alice", "password": "pw-alice",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("alice 登录应 200，实际 %d", w.Code)
	}
	aliceCookie := sessionCookieFrom(t, w)

	// 1) 登录态可访问：仅含自己的令牌（id 降序）。
	w = doJSON(t, r, http.MethodGet, "/api/token/mine", aliceCookie, nil)
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
		t.Fatalf("alice 应仅见自己的 2 枚令牌，实际 total=%d len=%d body=%s",
			list.Data.Total, len(list.Data.Items), w.Body.String())
	}
	for _, item := range list.Data.Items {
		if uid, _ := item["user_id"].(float64); int64(uid) != alice.ID {
			t.Fatalf("mine 应仅含当前用户令牌: user_id=%v", item["user_id"])
		}
	}

	// 2) 字段白名单：密钥材料与管理配置字段不得出现在响应中。
	for _, banned := range []string{"key_hash", `"key":`, "tpm_rpm", "tags", "allow_ips"} {
		if strings.Contains(w.Body.String(), banned) {
			t.Fatalf("mine 响应不得含 %s: %s", banned, w.Body.String())
		}
	}

	// 3) 越权探测：user_id 指向他人也被忽略，强制当前用户作用域。
	w = doJSON(t, r, http.MethodGet, "/api/token/mine?user_id="+itoa(bob.ID), aliceCookie, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":2`) {
		t.Fatalf("user_id 参数应被忽略（恒当前用户作用域），实际 %d %s", w.Code, w.Body.String())
	}

	// 4) 越权管理面：普通用户访问管理列表 GET /api/token 403。
	w = doJSON(t, r, http.MethodGet, "/api/token", aliceCookie, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问管理列表应 403，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 5) 未登录 401。
	if w := doJSON(t, r, http.MethodGet, "/api/token/mine", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
}
