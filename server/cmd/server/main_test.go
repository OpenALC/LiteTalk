package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// 验证 healthz handler 返回 200 与正确的 JSON 结构
func TestHealthzHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("期望状态码 200，实际 %d", res.StatusCode)
	}

	if ct := res.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("期望 Content-Type application/json; charset=utf-8，实际 %q", ct)
	}

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}

	if body.Status != "ok" {
		t.Fatalf("期望 status=ok，实际 %q", body.Status)
	}
	if body.Version == "" {
		t.Fatal("version 不应为空")
	}
}

// TestValidateJWTSecret 生产模式密钥准入：空/示例值/短密钥拒绝，dev 模式全放行。
func TestValidateJWTSecret(t *testing.T) {
	// dev 模式不做限制（本地开发允许回落开发密钥）。
	for _, s := range []string{"", "short", "CHANGE_ME", "litetalk-dev-secret-change-me", "x"} {
		if err := validateJWTSecret(s, true); err != nil {
			t.Fatalf("dev 模式密钥 %q 应放行，实际 %v", s, err)
		}
	}

	// 生产模式：弱密钥全部拒绝。
	weak := []string{"", "CHANGE_ME", "litetalk-dev-secret-change-me", "short-secret", "0123456789abcdef0123456789abcde"} // 31 字节
	for _, s := range weak {
		if err := validateJWTSecret(s, false); err == nil {
			t.Fatalf("生产模式弱密钥 %q 应拒绝", s)
		}
	}

	// 生产模式：≥32 字节随机串放行。
	if err := validateJWTSecret("0123456789abcdef0123456789abcdef", false); err != nil {
		t.Fatalf("32 字节密钥应放行，实际 %v", err)
	}
	if err := validateJWTSecret("a-much-longer-random-production-secret-value", false); err != nil {
		t.Fatalf("长随机密钥应放行，实际 %v", err)
	}
}

// TestResolveMigrations 验证迁移 FS 选择：默认 embed，LITETALK_MIGRATIONS_DIR 覆盖磁盘。
func TestResolveMigrations(t *testing.T) {
	// 默认走 embed。
	t.Setenv("LITETALK_MIGRATIONS_DIR", "")
	got := resolveMigrations()
	if _, ok := got.(fs.FS); !ok {
		t.Fatal("应返回 fs.FS")
	}
	entries, err := fs.ReadDir(got, ".")
	if err != nil {
		t.Fatalf("embed FS 不可读: %v", err)
	}
	var hasUp bool
	for _, e := range entries {
		if e.Name() == "0001_init.up.sql" {
			hasUp = true
		}
	}
	if !hasUp {
		t.Fatal("embed FS 应包含 0001_init.up.sql")
	}

	// 磁盘覆盖模式。
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/0001_init.up.sql", []byte("SELECT 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITETALK_MIGRATIONS_DIR", dir)
	got = resolveMigrations()
	if _, err := fs.ReadFile(got, "0001_init.up.sql"); err != nil {
		t.Fatalf("磁盘 FS 应可读: %v", err)
	}
}
