package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// TestValidateTURNSecret 显式配置的 TURN 密钥准入（#25）：弱值/短值拒绝，
// 未设置放行（走随机生成），dev 模式全放行。
func TestValidateTURNSecret(t *testing.T) {
	weak := []string{"CHANGE_ME", "litetalk-dev-secret-change-me", "short-secret", strings.Repeat("a", 31)}
	for _, s := range weak {
		if err := validateTURNSecret(s, true); err != nil {
			t.Fatalf("dev 模式 %q 应放行，实际 %v", s, err)
		}
		if err := validateTURNSecret(s, false); err == nil {
			t.Fatalf("生产模式弱密钥 %q 应拒绝", s)
		}
	}
	if err := validateTURNSecret("", false); err != nil {
		t.Fatalf("未设置应放行（走随机生成），实际 %v", err)
	}
	if err := validateTURNSecret(strings.Repeat("a", 32), false); err != nil {
		t.Fatalf("32 字节密钥应放行，实际 %v", err)
	}
}

// TestResolveTURNSecret 未设置且内置 TURN 启用时随机生成 64 位 hex；未启用时
// 保持空串（不额外消耗随机源）；显式强值原样返回。
func TestResolveTURNSecret(t *testing.T) {
	secret, generated, err := resolveTURNSecret("", true, false)
	if err != nil {
		t.Fatalf("未设置应随机生成: %v", err)
	}
	if !generated || len(secret) != 64 {
		t.Fatalf("应生成 64 位 hex 随机串，实际 generated=%v len=%d", generated, len(secret))
	}
	again, _, err := resolveTURNSecret("", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if secret == again {
		t.Fatal("两次随机生成不应相同")
	}

	secret, generated, err = resolveTURNSecret("", false, false)
	if err != nil || generated || secret != "" {
		t.Fatalf("TURN 关闭且未设置时应保持空串，实际 secret=%q generated=%v err=%v", secret, generated, err)
	}

	const explicit = "0123456789abcdef0123456789abcdef"
	secret, generated, err = resolveTURNSecret(explicit, true, false)
	if err != nil || generated || secret != explicit {
		t.Fatalf("显式强密钥应原样返回，实际 secret=%q generated=%v err=%v", secret, generated, err)
	}

	if _, _, err := resolveTURNSecret("CHANGE_ME", true, false); err == nil {
		t.Fatal("生产模式显式弱密钥应拒绝")
	}
}

// TestValidateSFUIP 生产模式拒环回地址（#22）。
func TestValidateSFUIP(t *testing.T) {
	// dev 模式全放行。
	for _, ip := range []string{"127.0.0.1", "::1", "10.0.0.1", "bad-ip"} {
		if err := validateSFUIP(ip, true); err != nil {
			t.Fatalf("dev 模式 %q 应放行，实际 %v", ip, err)
		}
	}
	// 生产：环回拒绝。
	for _, ip := range []string{"127.0.0.1", "127.0.0.53", "::1"} {
		if err := validateSFUIP(ip, false); err == nil {
			t.Fatalf("生产模式环回 %q 应拒绝", ip)
		}
	}
	// 生产：非法 IP 拒绝。
	if err := validateSFUIP("not-an-ip", false); err == nil {
		t.Fatal("非法 IP 应拒绝")
	}
	// 生产：公网/内网 IP 放行。
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "8.8.8.8", "2001:db8::1"} {
		if err := validateSFUIP(ip, false); err != nil {
			t.Fatalf("非环回 %q 应放行，实际 %v", ip, err)
		}
	}
}
func TestResolveMigrations(t *testing.T) {
	// 默认走 embed：MySQL 侧读根目录，SQLite 侧读 sqlite/ 子目录。
	t.Setenv("LITETALK_MIGRATIONS_DIR", "")
	myFS := resolveMigrationsMySQL()
	if _, ok := myFS.(fs.FS); !ok {
		t.Fatal("应返回 fs.FS")
	}
	entries, err := fs.ReadDir(myFS, ".")
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
	sqFS := resolveMigrationsSQLite()
	if _, err := fs.ReadFile(sqFS, "0001_init.up.sql"); err != nil {
		t.Fatalf("SQLite 子目录应可读: %v", err)
	}

	// 磁盘覆盖模式：布局与 server/migrations/ 一致（根 = MySQL，sqlite/ = SQLite）。
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/0001_init.up.sql", []byte("SELECT 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir+"/sqlite", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/sqlite/0001_init.up.sql", []byte("SELECT 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITETALK_MIGRATIONS_DIR", dir)
	if _, err := fs.ReadFile(resolveMigrationsMySQL(), "0001_init.up.sql"); err != nil {
		t.Fatalf("磁盘 MySQL FS 应可读: %v", err)
	}
	body, err := fs.ReadFile(resolveMigrationsSQLite(), "0001_init.up.sql")
	if err != nil {
		t.Fatalf("磁盘 SQLite FS 应可读: %v", err)
	}
	if string(body) != "SELECT 2" {
		t.Fatalf("SQLite FS 应读到子目录文件，实际 %q", body)
	}
}

// TestPickStoreKind 选型推断链（§0.3）：显式 > MYSQL_DSN 非空 > 默认 sqlite。
func TestPickStoreKind(t *testing.T) {
	t.Setenv("LITETALK_STORE", "")
	t.Setenv("LITETALK_MYSQL_DSN", "")
	if got := pickStoreKind(); got != "sqlite" {
		t.Fatalf("都未设应默认 sqlite，实际 %q", got)
	}
	t.Setenv("LITETALK_MYSQL_DSN", "user:pass@tcp(127.0.0.1:3306)/x")
	if got := pickStoreKind(); got != "mysql" {
		t.Fatalf("有 DSN 应选 mysql，实际 %q", got)
	}
	t.Setenv("LITETALK_STORE", "sqlite")
	if got := pickStoreKind(); got != "sqlite" {
		t.Fatalf("显式指定应优先，实际 %q", got)
	}
	t.Setenv("LITETALK_STORE", " MySQL ")
	if got := pickStoreKind(); got != "mysql" {
		t.Fatalf("大小写与空白应归一，实际 %q", got)
	}
	t.Setenv("LITETALK_STORE", "mem")
	if got := pickStoreKind(); got != "mem" {
		t.Fatalf("mem 应透传由 main 拒绝/放行，实际 %q", got)
	}
}
