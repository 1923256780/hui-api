package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleHealth 固化 /health 契约：200 + JSON，且 status=ok。
func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handleHealth(rec, req)

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
}

// TestNewMuxHealthRoute 验证路由注册，防止后续重构路由表时丢掉 /health。
func TestNewMuxHealthRoute(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("请求 /health 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望状态码 200，实际为 %d", resp.StatusCode)
	}
}
