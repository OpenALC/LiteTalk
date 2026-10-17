package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// TestDeviceTypeWhitelist 设备类型白名单（P2-2）：空→desktop；白名单内原样放行；
// 白名单外（含大小写变体与"像设备名"的串）一律拒绝——与 auth.DeviceTypes 共用一份。
func TestDeviceTypeWhitelist(t *testing.T) {
	if d, ok := deviceType(""); !ok || d != "desktop" {
		t.Fatalf("空设备应回落 desktop: %q %v", d, ok)
	}
	if d, ok := deviceType("mobile"); !ok || d != "mobile" {
		t.Fatalf("mobile 应放行: %q %v", d, ok)
	}
	for _, bad := range []string{"tablet", "Desktop", "DESKTOP", "web", "desktop2", "mobile ", " desktop"} {
		if _, ok := deviceType(bad); ok {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

// TestLoginBadDevice 非法设备类型经 HTTP 入口 400（先校验设备，后查凭据）。
func TestLoginBadDevice(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "dev@ex.com", "password": "password123", "device": "tablet",
	}, http.StatusBadRequest)
}

// TestMobileLoginRefresh mobile 全链路（P0-2 回归）：mobile 注册→refresh 旋转→logout。
// 修复前 refresh/logout 把设备类型写死 desktop，mobile 的 rt:{uid}:mobile 键永远取不中，
// 刷新必 401。本测试不注入 SessionIndex（sess 为 nil），与 F4 的会话矩阵互补。
func TestMobileLoginRefresh(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "m@ex.com", "purpose": "register",
	}, http.StatusOK)
	code, err := mem.Get(context.Background(), "code:register:m@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	reg := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "mobileUser", "email": "m@ex.com", "password": "password123",
		"code": code, "device": "mobile",
	}, http.StatusCreated)
	mobileRefresh := reg["refresh_token"].(string)
	if _, dev, _, ok := auth.ParseRefreshToken(mobileRefresh); !ok || dev != "mobile" {
		t.Fatalf("注册令牌应带 mobile 段: %q %v", dev, ok)
	}

	// refresh 旋转成功（修复前按 desktop 取键必 401）。
	rot := postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": mobileRefresh,
	}, http.StatusOK)
	newRefresh := rot["refresh_token"].(string)
	if newRefresh == mobileRefresh {
		t.Fatal("refresh 应旋转出新令牌")
	}
	// 旧令牌重放 → 401 session_replayed（CAS 比对失败）。
	if resp := postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": mobileRefresh,
	}, http.StatusUnauthorized); resp["code"] != "session_replayed" {
		t.Fatalf("code = %v, want session_replayed", resp["code"])
	}

	// logout 吊销该设备 → 200，之后 refresh 失效。
	postJSON(t, h, "/api/v1/auth/logout", "", map[string]string{
		"refresh_token": newRefresh,
	}, http.StatusOK)
	postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": newRefresh,
	}, http.StatusUnauthorized)
}

// TestRefreshSessionReplacedByDevice 会话顶替按令牌自带设备类型判定（P0-2 回归）：
// mobile 会话被顶替后旧令牌 refresh → 401；desktop 令牌不受影响。
// 修复前 sess.Get 写死 desktop，mobile 用户的会话比对永远取错键。
// 令牌与会话直接构造（issueTokens 的 sid 遮蔽由 F1 修复，不依赖其产物）。
func TestRefreshSessionReplacedByDevice(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	srv := New(Config{
		Store:   mem,
		JWT:     auth.NewJWT("test-secret"),
		Refresh: auth.NewRefreshStore(mem),
		Codes:   auth.NewEmailCodes(mem),
		Limits:  auth.NewRateLimit(mem),
		Session: auth.NewSessionIndex(mem, mem),
	})
	h := srv.Handler()

	u := &store.User{Email: "sd@ex.com", Nickname: "sessionDev", Tag: "0001", PasswordHash: "x"}
	if err := mem.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	uid := u.ID

	// mobile 当前会话与令牌；另备一个旧 sid 的令牌（模拟被顶替前的会话）。
	curSid, err := srv.sess.Upsert(ctx, uid, "mobile", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	staleTok, err := srv.refresh.IssueWithUID(ctx, uid, "mobile", "dddddddddddddddddddddddddddddddd")
	if err != nil {
		t.Fatal(err)
	}
	goodTok, err := srv.refresh.IssueWithUID(ctx, uid, "mobile", curSid)
	if err != nil {
		t.Fatal(err)
	}
	// desktop 会话与令牌（与 mobile 互不干扰）。
	deskSid, err := srv.sess.Upsert(ctx, uid, "desktop", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	deskTok, err := srv.refresh.IssueWithUID(ctx, uid, "desktop", deskSid)
	if err != nil {
		t.Fatal(err)
	}

	// 旧 mobile 令牌 refresh → 401 session_replaced（Get 按令牌 dev=mobile 取会话，sid 不符）。
	if resp := postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": staleTok,
	}, http.StatusUnauthorized); resp["code"] != "session_replaced" {
		t.Fatalf("code = %v, want session_replaced", resp["code"])
	}

	// 新 mobile 令牌可旋转，且签出的 access 令牌带 mobile/sid，能过鉴权中间件。
	rot := postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": goodTok,
	}, http.StatusOK)
	getJSON(t, h, "/api/v1/users/me", rot["access_token"].(string), http.StatusOK)

	// desktop 令牌同样可旋转。
	postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": deskTok,
	}, http.StatusOK)
}
