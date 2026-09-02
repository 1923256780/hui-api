package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/store"
)

// newTestRouter 用临时库构造与 run() 相同的路由，固化 /health 与 /api/status 契约。
func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	schemaVersion, err := st.Migrate()
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	rt, err := config.NewRuntime(st)
	if err != nil {
		t.Fatalf("构造 Runtime 失败: %v", err)
	}
	eng, gw, err := newRouter(st, rt, schemaVersion)
	if err != nil {
		t.Fatalf("组装路由失败: %v", err)
	}
	t.Cleanup(func() { gw.Close() })
	return eng
}

// TestHandleHealth 固化 /health 契约：200 + JSON，且 status=ok。
func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	newTestRouter(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望状态码 200，实际为 %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("健康检查响应不是合法 JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("期望 status 为 ok，实际为 %q", body["status"])
	}
	if body["version"] == "" {
		t.Fatal("健康检查响应缺少 version 字段")
	}
}

// TestStatusEndpoint 固化 /api/status 契约：管理面统一包裹，含版本与 schema 版本。
func TestStatusEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()

	newTestRouter(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望状态码 200，实际为 %d", rec.Code)
	}
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Version       string `json:"version"`
			SchemaVersion int64  `json:"schema_version"`
			ConfigVersion int64  `json:"config_version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("状态响应不是合法 JSON: %v", err)
	}
	if !body.Success {
		t.Fatal("success 应为 true")
	}
	if body.Data.Version == "" {
		t.Fatal("data.version 不应为空")
	}
	if body.Data.SchemaVersion < 1 {
		t.Fatalf("data.schema_version 应 >=1，实际 %d", body.Data.SchemaVersion)
	}
	if body.Data.ConfigVersion < 1 {
		t.Fatalf("data.config_version 应 >=1（首次加载），实际 %d", body.Data.ConfigVersion)
	}
}

// TestRandomSecret 随机密钥应为 64 位 hex。
func TestRandomSecret(t *testing.T) {
	secret, err := randomSecret()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	if len(secret) != 64 {
		t.Fatalf("期望 64 字符 hex，实际 %d", len(secret))
	}
}
