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
	srv := New(Config{
		Store:             mem,
		JWT:               auth.NewJWT("test-secret"),
		Refresh:           auth.NewRefreshStore(mem),
		Codes:             auth.NewEmailCodes(mem),
		Limits:            auth.NewRateLimit(mem),
		AuthMode:          AuthModeSimple,
		Session:           auth.NewSessionIndex(mem, mem),
		Presence:          mem,
		SelfHost:          true,
		BootstrapRequired: func() bool { return bootstrap.Required(context.Background(), mem) },
	})
	return srv, mem
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

// itoaJSON 把 JSON number（float64）转成路径用的十进制串。
func itoaJSON(v any) string {
	f, ok := v.(float64)
	if !ok {
		return "0"
	}
	return strconv.FormatInt(int64(f), 10)
}
