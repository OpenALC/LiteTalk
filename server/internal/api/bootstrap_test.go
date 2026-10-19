package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/bootstrap"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// newBootstrapTestServer 与生产同构：BootstrapRequired 接 bootstrap.Required，
// Presence 即 KV（消费标记经类型断言取回，见 api/bootstrap.go）。
func newBootstrapTestServer(t *testing.T) (*Server, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	return newBootstrapServer(t, mem, mem), mem
}

// newBootstrapServer 允许注入独立 Store/KV：模拟「重启后 KV 丢失、库仍在」。
func newBootstrapServer(t *testing.T, st store.Store, kv store.KV) *Server {
	t.Helper()
	return New(Config{
		Store:             st,
		JWT:               auth.NewJWT("test-secret"),
		Refresh:           auth.NewRefreshStore(kv),
		Codes:             auth.NewEmailCodes(kv),
		Limits:            auth.NewRateLimit(kv),
		AuthMode:          AuthModeSimple,
		Session:           auth.NewSessionIndex(st, kv),
		Presence:          kv,
		SelfHost:          true,
		BootstrapRequired: func() bool { return bootstrap.Required(context.Background(), kv, st) },
	})
}

// TestRegisterBootstrapCode 带码注册 → 认领默认服务器；码一次性（#27）。
func TestRegisterBootstrapCode(t *testing.T) {
	srv, mem := newBootstrapTestServer(t)
	h := srv.Handler()
	code := bootstrap.Code("test-secret")

	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "机主", "email": "root@ex.com", "password": "password123", "bootstrap": code,
	}, http.StatusCreated)
	access, _ := resp["access_token"].(string)
	if access == "" {
		t.Fatal("注册未返回令牌")
	}

	// 默认服务器 + 文字/语音频道。
	list, _ := getJSON(t, h, "/api/v1/servers", access, http.StatusOK).([]any)
	if len(list) != 1 {
		t.Fatalf("应恰好一个默认服务器，实际 %d", len(list))
	}
	s0 := list[0].(map[string]any)
	if s0["name"] != bootstrap.DefaultServerName {
		t.Fatalf("服务器名 = %v，want %v", s0["name"], bootstrap.DefaultServerName)
	}
	chs, _ := getJSON(t, h, "/api/v1/servers/"+itoaJSON(s0["id"])+"/channels", access, http.StatusOK).(map[string]any)
	if arr, _ := chs["channels"].([]any); len(arr) != 2 {
		t.Fatalf("应建文字/语音两频道，实际 %v", chs)
	}

	// 同一码再用 → 400 bad_bootstrap，且不产生半成品账号。
	bad := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "路人", "email": "second@ex.com", "password": "password123", "bootstrap": code,
	}, http.StatusBadRequest)
	if bad["code"] != "bad_bootstrap" {
		t.Fatalf("code = %v, want bad_bootstrap", bad["code"])
	}
	if _, err := mem.UserByEmail(context.Background(), "second@ex.com"); err != store.ErrNotFound {
		t.Fatalf("被拒注册不应建用户，实际 err=%v", err)
	}

	// 消费后 bootstrap_required 翻 false。
	cfg, _ := getJSON(t, h, "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfg["bootstrap_required"] != false {
		t.Fatalf("消费后 bootstrap_required 应为 false，实际 %v", cfg["bootstrap_required"])
	}
}

// TestRegisterBootstrapInvalid 乱填邀请码 → 400，不建用户；不带码注册不建服务器。
func TestRegisterBootstrapInvalid(t *testing.T) {
	srv, mem := newBootstrapTestServer(t)
	h := srv.Handler()

	bad := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "错码", "email": "wrong@ex.com", "password": "password123",
		"bootstrap": "deadbeefdeadbeefdeadbeefdeadbeef",
	}, http.StatusBadRequest)
	if bad["code"] != "bad_bootstrap" {
		t.Fatalf("code = %v, want bad_bootstrap", bad["code"])
	}
	if _, err := mem.UserByEmail(context.Background(), "wrong@ex.com"); err != store.ErrNotFound {
		t.Fatalf("错码注册不应建用户，实际 err=%v", err)
	}

	// 不带码：普通注册，不自动建服务器。
	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "普通用户", "email": "plain@ex.com", "password": "password123",
	}, http.StatusCreated)
	access, _ := resp["access_token"].(string)
	list, _ := getJSON(t, h, "/api/v1/servers", access, http.StatusOK).([]any)
	if len(list) != 0 {
		t.Fatalf("不带码注册不应建服务器，实际 %d", len(list))
	}
	// 码未消费 → bootstrap_required 仍为 true。
	cfg, _ := getJSON(t, h, "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfg["bootstrap_required"] != true {
		t.Fatalf("码未消费 bootstrap_required 应为 true，实际 %v", cfg["bootstrap_required"])
	}
}

// TestRegisterBootstrapRejectedAfterRestart #27 P1：消费标记只在进程内 KV，
// 重启（新 KV、同一数据库）后同一个码必须仍被拒绝，且不产生第二个默认服务器。
func TestRegisterBootstrapRejectedAfterRestart(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem() // 持久层：跨「重启」保留
	code := bootstrap.Code("test-secret")

	// 第一次启动：KV 与库同进程，带码注册成功并建默认服务器。
	h1 := newBootstrapServer(t, st, st).Handler()
	postJSON(t, h1, "/api/v1/auth/register", "", map[string]string{
		"nickname": "机主", "email": "root@ex.com", "password": "password123", "bootstrap": code,
	}, http.StatusCreated)
	owner, err := st.UserByEmail(ctx, "root@ex.com")
	if err != nil {
		t.Fatalf("首次注册应建用户: %v", err)
	}

	// 模拟重启：MemKV 全丢（新实例），SQLite/库文件还在。
	kv2 := store.NewMemKV()
	h2 := newBootstrapServer(t, st, kv2).Handler()

	// 持久判据：库内已有服务器 → bootstrap_required 必须为 false。
	cfg, _ := getJSON(t, h2, "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfg["bootstrap_required"] != false {
		t.Fatalf("重启后 bootstrap_required 应反映持久状态为 false，实际 %v", cfg["bootstrap_required"])
	}

	// 同一个码再注册 → 400 bad_bootstrap，不建用户。
	bad := postJSON(t, h2, "/api/v1/auth/register", "", map[string]string{
		"nickname": "路人", "email": "second@ex.com", "password": "password123", "bootstrap": code,
	}, http.StatusBadRequest)
	if bad["code"] != "bad_bootstrap" {
		t.Fatalf("重启后复用邀请码 code = %v，want bad_bootstrap", bad["code"])
	}
	if _, err := st.UserByEmail(ctx, "second@ex.com"); err != store.ErrNotFound {
		t.Fatalf("被拒注册不应建用户，实际 err=%v", err)
	}

	// 仍只有一个默认服务器。
	servers, err := st.ServersOfUser(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("重启后不应产生第二个默认服务器，实际 %d", len(servers))
	}
}

// itoaJSON 把响应里的 ID（字符串或 JSON number）转成路径用的十进制串。
func itoaJSON(v any) string {
	return strconv.FormatInt(asID(v), 10)
}
