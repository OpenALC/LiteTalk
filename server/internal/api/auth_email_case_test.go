package api

// auth_email_case_test.go — 邮箱大小写归一（#119）：本改动之前写入的混合大小写
// 存量账号必须仍能登录，且不能被同名小写邮箱重复注册。

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/migrations"
)

// newSQLiteTestServer 用真 SQLite 存储起一个 simple 模式 API Server：邮箱大小写
// 的唯一性在 SQLite 上是二进制比较，Mem 的 EqualFold 语义会掩盖这里的缺陷。
func newSQLiteTestServer(t *testing.T) (*Server, *store.SQLite) {
	t.Helper()
	t.Setenv("LITETALK_SQLITE_PATH", "") // 避免环境变量覆盖 TempDir 路径
	sq, err := store.OpenSQLite(filepath.Join(t.TempDir(), "api-email-case.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	migFS, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		t.Fatalf("fs.Sub sqlite: %v", err)
	}
	if err := sq.Migrate(context.Background(), migFS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	kv := store.NewMem() // 会话/验证码/限流仍走内存 KV
	return New(Config{
		Store:    sq,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(kv),
		Codes:    auth.NewEmailCodes(kv),
		Limits:   auth.NewRateLimit(kv),
		AuthMode: AuthModeSimple,
		Session:  auth.NewSessionIndex(kv, kv),
		Presence: kv,
	}), sq
}

// TestLoginLegacyMixedCaseEmail 存量混合大小写账号用规范化邮箱登录成功。
func TestLoginLegacyMixedCaseEmail(t *testing.T) {
	srv, mem := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateUser(context.Background(), &store.User{
		Email: "Legacy.Mixed@Example.COM", Nickname: "legacy", Tag: "0001", PasswordHash: hash,
	}); err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "legacy.mixed@example.com", "password": "password123",
	}, http.StatusOK)
	if resp["access_token"] == "" {
		t.Fatal("混合大小写存量账号登录未返回令牌")
	}
}

// TestRegisterRejectsCaseVariantOfLegacyEmail 存量混合大小写邮箱已注册时，
// 用小写形态再注册必须 409，不能建出重复账号（SQLite 唯一索引区分大小写，
// 单纯的新入口归一小写化不会与旧行撞键）。
func TestRegisterRejectsCaseVariantOfLegacyEmail(t *testing.T) {
	srv, sq := newSQLiteTestServer(t)
	h := srv.Handler()
	if err := sq.CreateUser(context.Background(), &store.User{
		Email: "Legacy.Mixed@Example.COM", Nickname: "legacy", Tag: "0001",
	}); err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "重复用户", "email": "legacy.mixed@example.com", "password": "password123",
	}, http.StatusConflict)
	if resp["code"] != "email_taken" {
		t.Fatalf("code = %v, want email_taken", resp["code"])
	}
}
