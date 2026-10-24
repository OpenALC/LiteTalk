package main

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// TestLinkPreviewFromEnv 链接预览开关（#42 B4）：默认开；off/0/false 关闭时
// 返回 nil（端点统一 404）；区间夹取不应触发 envInt 的非法值退出。
func TestLinkPreviewFromEnv(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	for _, off := range []string{"off", "0", "false", "OFF", " False "} {
		t.Setenv("LITETALK_LINK_PREVIEW", off)
		if f := linkPreviewFromEnv(lg); f != nil {
			t.Fatalf("LITETALK_LINK_PREVIEW=%q 应关闭功能", off)
		}
	}
	for _, on := range []string{"", "on", "1", "true"} {
		t.Setenv("LITETALK_LINK_PREVIEW", on)
		t.Setenv("LITETALK_LINK_PREVIEW_TIMEOUT_MS", "")
		t.Setenv("LITETALK_LINK_PREVIEW_MAX_BYTES", "")
		if f := linkPreviewFromEnv(lg); f == nil {
			t.Fatalf("LITETALK_LINK_PREVIEW=%q 应开启功能", on)
		}
	}
	// 越界值夹取后仍能构造（不 panic、不退出）。
	t.Setenv("LITETALK_LINK_PREVIEW", "on")
	t.Setenv("LITETALK_LINK_PREVIEW_TIMEOUT_MS", "10")
	t.Setenv("LITETALK_LINK_PREVIEW_MAX_BYTES", "1")
	if f := linkPreviewFromEnv(lg); f == nil {
		t.Fatal("越界配置应夹取而非关闭功能")
	}
	t.Setenv("LITETALK_LINK_PREVIEW_TIMEOUT_MS", "999999")
	t.Setenv("LITETALK_LINK_PREVIEW_MAX_BYTES", "999999999")
	if f := linkPreviewFromEnv(lg); f == nil {
		t.Fatal("越界配置应夹取而非关闭功能")
	}
}

// TestResolveCredentialURLs 下发给客户端的 TURN/STUN URL（#13 P2）：
// 显式配置优先；内置 TURN 启用回填 host；关闭时只有 dev 允许 loopback 回落，
// 生产模式返回空列表 + 告警。
func TestResolveCredentialURLs(t *testing.T) {
	// 显式配置原样透传，不回落。
	turn, stun, warn := resolveCredentialURLs("turn:a.example:3478", "stun:a.example:3478", "host", 3478, false)
	if len(turn) != 1 || turn[0] != "turn:a.example:3478" || len(stun) != 1 || stun[0] != "stun:a.example:3478" || warn != "" {
		t.Fatalf("显式配置应原样透传: turn=%v stun=%v warn=%q", turn, stun, warn)
	}

	// 内置 TURN 启用、两者都未配置：按 host+端口回填 udp/tcp 两条 TURN 与一条 STUN。
	turn, stun, warn = resolveCredentialURLs("", "", "voice.example.com", 3478, false)
	if len(turn) != 2 || len(stun) != 1 || warn != "" {
		t.Fatalf("内置 TURN 回填不符: turn=%v stun=%v warn=%q", turn, stun, warn)
	}
	if turn[0] != "turn:voice.example.com:3478?transport=udp" || stun[0] != "stun:voice.example.com:3478" {
		t.Fatalf("回填 URL 不符: turn=%v stun=%v", turn, stun)
	}

	// 内置 TURN 关闭 + 生产：不得回落 loopback，返回空列表与告警。
	turn, stun, warn = resolveCredentialURLs("", "", "voice.example.com", 0, false)
	if len(turn) != 0 || len(stun) != 0 {
		t.Fatalf("生产模式不应下发 loopback 回落: turn=%v stun=%v", turn, stun)
	}
	if warn == "" {
		t.Fatal("生产模式跳过回落时应返回启动告警")
	}

	// 内置 TURN 关闭 + dev：保留本机回落（本地联调）。
	turn, stun, warn = resolveCredentialURLs("", "", "127.0.0.1", 0, true)
	if len(turn) != 1 || turn[0] != "turn:127.0.0.1:3478" || len(stun) != 1 || stun[0] != "stun:127.0.0.1:3478" || warn != "" {
		t.Fatalf("dev 模式应回落本机: turn=%v stun=%v warn=%q", turn, stun, warn)
	}

	// 只配了一侧：turnPort=0 时生产不补齐另一侧（不凭空造 loopback），但保留显式值。
	turn, stun, warn = resolveCredentialURLs("turn:ext.example:3478", "", "host", 0, false)
	if len(turn) != 1 || len(stun) != 0 || warn == "" {
		t.Fatalf("只配 TURN 时生产应保留显式值并告警: turn=%v stun=%v warn=%q", turn, stun, warn)
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

// TestValidateSFUIP 生产模式拒环回与未指定地址（#22）。
func TestValidateSFUIP(t *testing.T) {
	// dev 模式全放行。
	for _, ip := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "10.0.0.1", "bad-ip"} {
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
	// 生产：未指定地址拒绝（通告 0.0.0.0/:: 会让客户端候选不可达）。
	for _, ip := range []string{"0.0.0.0", "::", "0:0:0:0:0:0:0:0"} {
		if err := validateSFUIP(ip, false); err == nil {
			t.Fatalf("生产模式未指定地址 %q 应拒绝", ip)
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

// TestSelectKVSQLiteWiring #83：SQLite 后端未配 Redis 时 KV 落同一个库文件，
// 重启（重新打开库）后仍能读回——接线层不走真实服务启动。
func TestSelectKVSQLiteWiring(t *testing.T) {
	t.Setenv("LITETALK_SQLITE_PATH", "")
	t.Setenv("LITETALK_MIGRATIONS_DIR", "")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "litetalk.db")

	sq, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := sq.Migrate(ctx, resolveMigrationsSQLite()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	kv, label, err := selectKV("sqlite", sq)
	if err != nil {
		t.Fatalf("selectKV(sqlite): %v", err)
	}
	if !strings.Contains(label, "SQLite") {
		t.Fatalf("SQLite 后端标签应说明持久化，实际 %q", label)
	}
	if _, ok := kv.(*store.SQLiteKV); !ok {
		t.Fatalf("SQLite 后端应返回 SQLiteKV，实际 %T", kv)
	}
	if err := kv.Set(ctx, "rt:1:desktop", "hash", time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 直接查 Store 的句柄：KV 与 Store 必须是同一个库。
	var v string
	if err := sq.DB().QueryRowContext(ctx,
		`SELECT kv_value FROM kv WHERE kv_key = ?`, "rt:1:desktop").Scan(&v); err != nil {
		t.Fatalf("同一 DB 应能读到 KV 行: %v", err)
	}
	if v != "hash" {
		t.Fatalf("同一 DB 读到 %q，want hash", v)
	}
	if err := sq.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重新打开同一文件，新建 SQLiteKV 应读回。
	sq2, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("重开 SQLite: %v", err)
	}
	defer sq2.Close()
	if err := sq2.Migrate(ctx, resolveMigrationsSQLite()); err != nil {
		t.Fatalf("重开迁移幂等: %v", err)
	}
	kv2, _, err := selectKV("sqlite", sq2)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := kv2.Get(ctx, "rt:1:desktop"); err != nil || got != "hash" {
		t.Fatalf("重启后 Get = %q/%v，want hash/nil", got, err)
	}
}

// TestSelectKVFallbacks mem（仅 dev）与 mysql（无 Redis）都退回进程内并说明；
// SQLite 缺少 Store 句柄或后端未知时必须报错而不是静默降级。
func TestSelectKVFallbacks(t *testing.T) {
	for _, kind := range []string{"mem", "mysql"} {
		kv, label, err := selectKV(kind, nil)
		if err != nil {
			t.Fatalf("selectKV(%s): %v", kind, err)
		}
		if _, ok := kv.(*store.MemKV); !ok {
			t.Fatalf("%s 后端应返回 MemKV，实际 %T", kind, kv)
		}
		if !strings.Contains(label, "进程内") || !strings.Contains(label, "重启即丢") {
			t.Fatalf("%s 标签应明确不持久，实际 %q", kind, label)
		}
	}
	if _, _, err := selectKV("sqlite", nil); err == nil {
		t.Fatal("SQLite 后端缺少 Store 应报错")
	}
	if _, _, err := selectKV("bogus", nil); err == nil {
		t.Fatal("未知后端应报错")
	}
}

// TestHealthOnlyHandler ACME 模式的回环健康面（#101 P1）：只提供
// GET /api/v1/healthz，形状与主端点一致；其他路径 404，不暴露管理 API。
func TestHealthOnlyHandler(t *testing.T) {
	h := healthOnlyHandler("v9.9.9-alpha")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz 状态码 = %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q，want application/json", ct)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz 不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if body.Status != "ok" || body.Version != "v9.9.9-alpha" {
		t.Fatalf("healthz 形状不符: %+v", body)
	}

	// 回环面只暴露 healthz：其余路径一律 404。
	for _, path := range []string{"/api/v1/config", "/api/v1/users/me", "/ws", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404，实际 %d", path, rec.Code)
		}
	}
}
