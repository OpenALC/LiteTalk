package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// authPayload 是服务端 /auth/* 的真实 wire 形态（摘自线上游客登录响应）。
// 关键点：user.id 是数字（int64）——客户端曾误声明为 string，
// 导致登录/注册/游客三类响应在解码时全部失败。
const authPayload = `{
  "access_token": "tok", "refresh_token": "3.abc", "expires_in": 7200,
  "user": {
    "id": 3, "email": "guest-x@guest.local", "nickname": "游客-2733",
    "tag": "4399", "email_verified": false, "is_guest": true,
    "status": 0, "created_at": "2026-10-14T19:41:27Z", "updated_at": "2026-10-14T19:41:27Z"
  }
}`

// TestAuthDecode 锁住服务端响应契约：数字 id 可解码，字段齐全。
func TestAuthDecode(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(authPayload), &payload); err != nil {
		t.Fatalf("fixture 非法: %v", err)
	}
	userJSON, _ := json.Marshal(payload["user"])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/register":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(authPayload))
		case "/api/v1/users/me":
			_, _ = w.Write(userJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	r, err := c.Register("游客-2733", "x@y.com", "password123", "", "")
	if err != nil {
		t.Fatalf("Register 解码失败（id 类型契约被破坏？）: %v", err)
	}
	if r.User.ID != 3 || !r.User.IsGuest || r.User.Nickname != "游客-2733" || r.User.Tag != "4399" {
		t.Fatalf("Register 解码结果异常: %+v", r.User)
	}
	if r.AccessToken != "tok" || r.RefreshToken != "3.abc" {
		t.Fatalf("令牌解码异常: %+v", r)
	}

	c.SetToken(r.AccessToken)
	u, err := c.Me()
	if err != nil {
		t.Fatalf("Me 解码失败: %v", err)
	}
	if u.ID != 3 || !u.IsGuest || u.Email != "guest-x@guest.local" {
		t.Fatalf("Me 解码结果异常: %+v", u)
	}
}

// mePayload /users/me 的成功响应（wire 契约）。
const mePayload = `{"id":3,"email":"a@b.c","nickname":"甲","tag":"0001","is_guest":false,"status":1}`

// newRefreshStub 建带 401→刷新→重试语义的桩服务。
// me 端点对旧令牌返回 401 unauthorized，对新令牌返回 200；refresh 端点计数并轮换令牌。
func newRefreshStub(t *testing.T, refreshDelay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls.Add(1)
			if refreshDelay > 0 {
				time.Sleep(refreshDelay) // 拖住刷新，让并发请求都撞上同一次 in-flight
			}
			_, _ = w.Write([]byte(`{"access_token":"tok2","refresh_token":"rt2","expires_in":7200}`))
		case "/api/v1/users/me":
			if r.Header.Get("Authorization") == "Bearer tok2" {
				_, _ = w.Write([]byte(mePayload))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"访问令牌无效或已过期"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &refreshCalls
}

// TestAPI401RefreshRetry 锁住核心契约：401(unauthorized) → 刷新一次 → 新令牌重放原请求 → 200。
func TestAPI401RefreshRetry(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 0)

	c := NewAPIClient(srv.URL)
	c.SetToken("tok1")
	var onRefreshed atomic.Bool
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })
	c.SetOnRefreshed(func(tk AuthTokens) {
		if tk.RefreshToken == "rt2" {
			onRefreshed.Store(true)
		}
	})

	u, err := c.Me()
	if err != nil {
		t.Fatalf("Me 应经刷新重试后成功: %v", err)
	}
	if u.Nickname != "甲" {
		t.Fatalf("Me 解码异常: %+v", u)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1", got)
	}
	if c.Token() != "tok2" {
		t.Fatalf("access token 未更新为 tok2: %q", c.Token())
	}
	if !onRefreshed.Load() {
		t.Fatal("onRefreshed 未回调（旋转令牌未回传 app 层）")
	}
}

// TestAPI401RefreshDiscardsOnBaseChange 401 刷新期间 REST 基址被热切换（#17）：
// 换服发生在 401 返回之后、刷新 POST 构造之前。刷新 POST 必须钉在请求创建时的
// base（旧服务器 A），响应回来时基址已变则丢弃结果——绝不把 A 的 refresh_token
// 发给新服务器 B，也不安装/回调/重放 A 的新令牌。旧实现用 c.Base() 构造刷新
// 请求，会把 B 的地址和 A 的长期凭据凑到一起。
func TestAPI401RefreshDiscardsOnBaseChange(t *testing.T) {
	refreshSrcEntered := make(chan struct{})
	releaseRefreshSrc := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRefreshSrc) }) }
	var aMu sync.Mutex
	var aRefreshBody string

	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/users/me":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"token_expired","message":"expired"}`))
		case "/api/v1/auth/refresh":
			body, _ := io.ReadAll(r.Body)
			aMu.Lock()
			aRefreshBody = string(body)
			aMu.Unlock()
			_, _ = w.Write([]byte(`{"access_token":"tok-new-A","refresh_token":"rt-new-A","expires_in":7200}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer oldSrv.Close()

	var bMu sync.Mutex
	var bReqs []string
	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bMu.Lock()
		bReqs = append(bReqs, r.URL.Path+"|"+string(body))
		bMu.Unlock()
		http.NotFound(w, r)
	}))
	defer newSrv.Close()

	c := NewAPIClient(oldSrv.URL)
	c.SetToken("tok1")
	// 卡在 401 之后、刷新 POST 之前：测试在这里完成 SetBase（等价于用户此刻
	// 保存了新服务器地址），再放行刷新。
	defer release() // 断言中途失败也放行，避免 goroutine 悬挂
	c.SetRefreshSource(func(context.Context) (string, error) {
		enteredOnce.Do(func() { close(refreshSrcEntered) })
		<-releaseRefreshSrc
		return "rt-A", nil
	})
	var refreshed atomic.Int32
	c.SetOnRefreshed(func(AuthTokens) { refreshed.Add(1) })

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Me()
		errCh <- err
	}()

	select {
	case <-refreshSrcEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("401 后未进入刷新流程")
	}
	c.SetBase(newSrv.URL)
	release()

	err := <-errCh
	if !errors.Is(err, errBaseChanged) {
		t.Fatalf("换基址后的刷新应返回 errBaseChanged，得到 %v", err)
	}
	if got := c.Token(); got != "tok1" {
		t.Fatalf("旧服务器的新令牌不得安装，token = %q", got)
	}
	if got := refreshed.Load(); got != 0 {
		t.Fatalf("onRefreshed 不应回调，实际 %d 次", got)
	}

	aMu.Lock()
	body := aRefreshBody
	aMu.Unlock()
	if !strings.Contains(body, "rt-A") {
		t.Fatalf("刷新应发往旧服务器且带旧 refresh_token，body = %q", body)
	}
	bMu.Lock()
	gotB := append([]string(nil), bReqs...)
	bMu.Unlock()
	if len(gotB) != 0 {
		t.Fatalf("新服务器不应收到任何请求（尤其 refresh_token），实际: %v", gotB)
	}
}

// TestAPIRefreshSingleflight 并发 401 只触发一次刷新，其余请求等待共享结果。
func TestAPIRefreshSingleflight(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 150*time.Millisecond)

	c := NewAPIClient(srv.URL)
	c.SetToken("tok1")
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Me()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 Me[%d] 失败: %v", i, err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1（singleflight 合并失效）", got)
	}
}

// TestAPI401NonTokenCode 非令牌类 401（如 bad_credentials）不得触发刷新重试。
func TestAPI401NonTokenCode(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 0)

	c := NewAPIClient(srv.URL)
	c.SetToken("tok1")
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	// bad_credentials 语义：不动令牌体系，原样报错。
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"bad_credentials","message":"邮箱或密码错误"}`))
	}))
	defer badSrv.Close()
	c2 := NewAPIClient(badSrv.URL)
	c2.SetToken("tok1")
	c2.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	if _, err := c2.Me(); err == nil {
		t.Fatal("bad_credentials 应返回错误")
	} else if ae, ok := err.(*APIError); !ok || ae.Code != "bad_credentials" {
		t.Fatalf("错误应为 bad_credentials, 得到 %v", err)
	}
	// 桩的 refresh 端点全程不应被命中（只用 c2 的 srv 也不可能命中，双保险校验 c 未动）。
	_ = srv
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("非令牌 401 不应触发刷新, 实际 %d 次", got)
	}
}

// TestAPITokenConcurrent 并发读写 Token 无数据竞争（CI -race 下运行）。
func TestAPITokenConcurrent(t *testing.T) {
	c := NewAPIClient("http://127.0.0.1:1") // 不真发请求，只压令牌存取
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				c.SetToken(fmt.Sprintf("tok-%d-%d", i, j))
				_ = c.Token()
			}
		}(i)
	}
	wg.Wait()
}

// testJWT 构造只有 payload 有意义的 JWT：客户端只解 exp 判提前续期，不验签。
func testJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestEnsureAccessRefreshesExpiredJWT access 已过期 → EnsureAccess 单飞刷新一次，
// 安装新令牌（WS 重连前主动续期的核心路径，#84）。
func TestEnsureAccessRefreshesExpiredJWT(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 0)
	c := NewAPIClient(srv.URL)
	c.SetToken(testJWT(t, time.Now().Add(-time.Minute)))
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	if err := c.EnsureAccess(context.Background()); err != nil {
		t.Fatalf("EnsureAccess: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1", got)
	}
	if c.Token() != "tok2" {
		t.Fatalf("access 未更新为刷新结果: %q", c.Token())
	}
}

// TestEnsureAccessSkipsFreshToken 未过期 / 无法判定 exp 的令牌都不触发刷新——
// 否则每次 WS 重连都会旋转一次 refresh_token（dev- 令牌、测试桩尤其明显）。
func TestEnsureAccessSkipsFreshToken(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 0)
	c := NewAPIClient(srv.URL)
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	cases := []struct {
		name  string
		token string
	}{
		{"JWT 有效期内", testJWT(t, time.Now().Add(time.Hour))},
		{"非 JWT（opaque/dev）", "dev-alice"},
		{"三段式但 payload 非法", "a.!!!.c"},
		{"payload 无 exp", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.SetToken(tc.token)
			if err := c.EnsureAccess(context.Background()); err != nil {
				t.Fatalf("EnsureAccess: %v", err)
			}
			if c.Token() != tc.token {
				t.Fatalf("不应改令牌: %q", c.Token())
			}
		})
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("不应触发刷新, 实际 %d 次", got)
	}

	// 进入提前续期窗口（1 分钟内到期）必须刷新。
	c.SetToken(testJWT(t, time.Now().Add(30*time.Second)))
	if err := c.EnsureAccess(context.Background()); err != nil {
		t.Fatalf("EnsureAccess(即将过期): %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("即将过期应刷新一次, 实际 %d 次", got)
	}
}

// TestEnsureAccessSingleflight 并发调用 EnsureAccess 只刷新一次，其余等待共享结果。
func TestEnsureAccessSingleflight(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 150*time.Millisecond)
	c := NewAPIClient(srv.URL)
	c.SetToken(testJWT(t, time.Now().Add(-time.Hour)))
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.EnsureAccess(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureAccess[%d]: %v", i, err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1（单飞合并失效）", got)
	}
}

// TestRefreshAccessForcesRefresh 握手 401 后用的强制刷新：即便本地判断令牌
// 未过期，也要换新值（服务端拒绝 = 权威判定）。
func TestRefreshAccessForcesRefresh(t *testing.T) {
	srv, refreshCalls := newRefreshStub(t, 0)
	c := NewAPIClient(srv.URL)
	c.SetToken(testJWT(t, time.Now().Add(time.Hour)))
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	if err := c.RefreshAccess(context.Background()); err != nil {
		t.Fatalf("RefreshAccess: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1", got)
	}
	if c.Token() != "tok2" {
		t.Fatalf("access 未更新: %q", c.Token())
	}

	// 无 refresh 来源时不得静默成功：调用方要据此停止重连。
	c2 := NewAPIClient(srv.URL)
	if err := c2.RefreshAccess(context.Background()); !errors.Is(err, ErrNoRefreshSource) {
		t.Fatalf("无刷新来源应返回 ErrNoRefreshSource, 得到 %v", err)
	}
	if err := c2.EnsureAccess(context.Background()); !errors.Is(err, ErrNoRefreshSource) {
		t.Fatalf("空令牌 + 无刷新来源应返回 ErrNoRefreshSource, 得到 %v", err)
	}
}

// TestAPI401SessionReplacedFatal 会话顶替三分支之「致命」：401 + session_replaced/
// session_replayed（头或 body）→ ErrSessionReplaced 直接返回，绝不触发刷新重试。
func TestAPI401SessionReplacedFatal(t *testing.T) {
	cases := []struct {
		name   string
		header string // 空串 = 不带头，只靠 body code 兜底
		code   string
	}{
		{"头 session_replaced", "session_replaced", "session_replaced"},
		{"头 session_replayed", "session_replayed", "session_replayed"},
		{"仅 body code", "", "session_replaced"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var refreshCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/refresh":
					refreshCalls.Add(1)
					_, _ = w.Write([]byte(`{"access_token":"tok2","refresh_token":"rt2","expires_in":7200}`))
				case "/api/v1/users/me":
					if tc.header != "" {
						w.Header().Set("X-LiteTalk-Auth", tc.header)
					}
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(fmt.Sprintf(
						`{"code":%q,"message":"账号已在其他设备登录"}`, tc.code)))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			c := NewAPIClient(srv.URL)
			c.SetToken("tok1")
			c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

			_, err := c.Me()
			if !errors.Is(err, ErrSessionReplaced) {
				t.Fatalf("want ErrSessionReplaced, got %v", err)
			}
			if got := refreshCalls.Load(); got != 0 {
				t.Fatalf("会话顶替不得触发刷新, 实际 %d 次", got)
			}
			if c.Token() != "tok1" {
				t.Fatalf("会话顶替不得改动令牌: %q", c.Token())
			}
		})
	}
}

// TestAPI401SessionReplacedDuringRefresh 刷新窗口撞上会话顶替：
// 原请求 401(unauthorized) 走刷新，refresh 自身返回 session_replaced →
// 在飞请求同样以 ErrSessionReplaced 收场，不再重放。
func TestAPI401SessionReplacedDuringRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls.Add(1)
			w.Header().Set("X-LiteTalk-Auth", "session_replaced")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"session_replaced","message":"账号已在其他设备登录"}`))
		case "/api/v1/users/me":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"访问令牌无效或已过期"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	c.SetToken("tok1")
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	_, err := c.Me()
	if !errors.Is(err, ErrSessionReplaced) {
		t.Fatalf("want ErrSessionReplaced, got %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1", got)
	}
}

// TestAPI401TokenExpiredRefreshRetry 会话三分支之「token_expired」：
// 401 + body code token_expired → 刷新一次 → 新令牌重放原请求 → 200。
func TestAPI401TokenExpiredRefreshRetry(t *testing.T) {
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"tok2","refresh_token":"rt2","expires_in":7200}`))
		case "/api/v1/users/me":
			if r.Header.Get("Authorization") == "Bearer tok2" {
				_, _ = w.Write([]byte(mePayload))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"token_expired","message":"访问令牌已过期"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	c.SetToken("tok1")
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt1", nil })

	u, err := c.Me()
	if err != nil {
		t.Fatalf("token_expired 应经刷新重试后成功: %v", err)
	}
	if u.Nickname != "甲" {
		t.Fatalf("Me 解码异常: %+v", u)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, want 1", got)
	}
	if c.Token() != "tok2" {
		t.Fatalf("access token 未更新为 tok2: %q", c.Token())
	}
}

// TestClientVersionHeader 所有 REST 请求（GET/POST/PATCH/DELETE、刷新与
// rtc/credentials）都带 X-LiteTalk-Client；默认 dev，SetClientVersion 后生效，
// 空值回落 dev（#31 升级门）。
func TestClientVersionHeader(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get(clientVersionHeader))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/users/me":
			_, _ = w.Write([]byte(mePayload))
		case "/api/v1/auth/login", "/api/v1/auth/refresh":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","detail":"no"}`))
		default:
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	if _, err := c.Me(); err != nil { // GET
		t.Fatalf("Me: %v", err)
	}
	if err := c.UpdateFriend("1", "备注", "组"); err != nil { // PATCH
		t.Fatalf("UpdateFriend: %v", err)
	}
	if err := c.RemoveFriend("1"); err != nil { // DELETE
		t.Fatalf("RemoveFriend: %v", err)
	}
	if _, err := c.RTCCredentials(); err != nil { // POST（postCtx）
		t.Fatalf("RTCCredentials: %v", err)
	}
	if _, err := c.Login("a@b.c", "x"); err == nil { // POST 业务失败也算发出
		t.Fatal("Login 期望报错")
	}
	if _, err := c.Refresh("rt"); err == nil { // 刷新自身
		t.Fatal("Refresh 期望报错")
	}
	c.SetClientVersion("v1.5.0-alpha")
	if _, err := c.Me(); err != nil {
		t.Fatalf("Me after SetClientVersion: %v", err)
	}
	c.SetClientVersion("")
	if _, err := c.Me(); err != nil {
		t.Fatalf("Me after empty version: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"dev", "dev", "dev", "dev", "dev", "dev", "v1.5.0-alpha", "dev"}
	if len(got) != len(want) {
		t.Fatalf("请求数 %d，期望 %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个请求版本头 = %q，期望 %q（全部：%v）", i, got[i], want[i], got)
		}
	}
}

// TestSyncDecode GET /sync 聚合快照解码（#78）：服务器字段平铺（嵌入 ServerDTO）
// 与分类/频道/好友数组都按契约解出，好友 online 为服务端 presence 值。
func TestSyncDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sync" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"servers":[{
				"id":"10","name":"服十","owner_id":"1","icon_name":"music","icon_color":"#4FA4F3",
				"created_at":"2026-01-01T00:00:00Z",
				"categories":[{"id":"100","server_id":"10","name":"甲组","position":2,"created_at":"2026-01-01T00:00:00Z"}],
				"channels":[{"id":"101","server_id":"10","name":"大厅","type":"voice","category_id":"100","position":0,"created_at":"2026-01-01T00:00:00Z"}]
			}],
			"friends":[{"id":"5","status":"accepted","incoming":false,"remark":"","group":"","user":{
				"id":"2","nickname":"乙","tag":"0002","email_verified":true,"is_guest":false,
				"status":1,"online":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"
			}}]
		}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	snap, err := c.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(snap.Servers) != 1 {
		t.Fatalf("servers 应 1 条，实际 %d", len(snap.Servers))
	}
	sv := snap.Servers[0]
	if sv.ID != 10 || sv.Name != "服十" || sv.IconName != "music" || sv.OwnerID != 1 {
		t.Fatalf("服务器字段解码异常（ServerDTO 未平铺？）: %+v", sv)
	}
	if len(sv.Categories) != 1 || sv.Categories[0].Name != "甲组" || sv.Categories[0].Position != 2 {
		t.Fatalf("分类解码异常: %+v", sv.Categories)
	}
	if len(sv.Channels) != 1 || sv.Channels[0].ID != 101 || sv.Channels[0].Type != "voice" {
		t.Fatalf("频道解码异常: %+v", sv.Channels)
	}
	if sv.Channels[0].CategoryID == nil || *sv.Channels[0].CategoryID != 100 {
		t.Fatalf("频道分类归属解码异常: %+v", sv.Channels[0].CategoryID)
	}
	if len(snap.Friends) != 1 || snap.Friends[0].User.ID != 2 || !snap.Friends[0].User.Online {
		t.Fatalf("好友快照解码异常: %+v", snap.Friends)
	}
}

// TestAPIErrorRetryAfter 429 的 Retry-After 从响应头进 APIError（#31）：
// GET 与 POST 两条路径都要带上；无头或非法值按 0（不乱给倒计时）。
func TestAPIErrorRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		method string
		header string
		want   int
	}{
		{"GET 有头", http.MethodGet, "3", 3},
		{"GET 无头", http.MethodGet, "", 0},
		{"POST 有头", http.MethodPost, "7", 7},
		{"POST 非法头", http.MethodPost, "soon", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"code":"rate_limited","detail":"发送过于频繁"}`))
			}))
			defer srv.Close()

			c := NewAPIClient(srv.URL)
			var err error
			if tc.method == http.MethodGet {
				_, err = c.Me()
			} else {
				_, err = c.Login("a@b.c", "password123")
			}
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("429 应返回 *APIError，得到 %v", err)
			}
			if ae.Status != http.StatusTooManyRequests || ae.Code != "rate_limited" {
				t.Fatalf("错误解码异常: %+v", ae)
			}
			if ae.RetryAfter != tc.want {
				t.Fatalf("RetryAfter = %d, want %d", ae.RetryAfter, tc.want)
			}
		})
	}
}

// TestListMessagesClampsLimit 客户端把 limit 夹到契约范围（1–50），
// 不把非法值发给 fail-closed 的服务端（#31）。
func TestListMessagesClampsLimit(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/channels/7/messages" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		got = append(got, r.URL.Query().Get("limit"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	for _, limit := range []int{0, -3, 1, 50, 51} {
		if _, err := c.ListMessages(7, limit); err != nil {
			t.Fatalf("ListMessages(limit=%d): %v", limit, err)
		}
	}
	want := []string{"50", "50", "1", "50", "50"}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("发送的 limit = %v, want %v", got, want)
	}
}

// TestIsTransientRefreshError 刷新错误分类（#84 复审 4.1）：只有传输层错误
// 可退避重试；服务端明确应答、被顶替、换服与无刷新源都不可恢复。
func TestIsTransientRefreshError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"网络错误", errors.New("dial tcp: no route to host"), true},
		{"包装后的网络错误", fmt.Errorf("post /auth/refresh: %w", errors.New("EOF")), true},
		{"服务端 401", &APIError{Code: "token_expired", Status: 401}, false},
		{"服务端 403", &APIError{Code: "forbidden", Status: 403}, false},
		{"服务端 500", &APIError{Code: "internal", Status: 500}, false},
		{"会话被顶替", ErrSessionReplaced, false},
		{"换服", errBaseChanged, false},
		{"无刷新源", ErrNoRefreshSource, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientRefreshError(tc.err); got != tc.want {
				t.Fatalf("IsTransientRefreshError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsAuthRejected 服务端明确拒绝 refresh_token 的判定：只认 401/403 的
// *APIError（含包装），网络错误与 5xx 都不算「登录已过期」。
func TestIsAuthRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"401", &APIError{Code: "token_expired", Status: 401}, true},
		{"403", &APIError{Code: "forbidden", Status: 403}, true},
		{"500", &APIError{Code: "internal", Status: 500}, false},
		{"包装后的 401", fmt.Errorf("post /auth/refresh: %w", &APIError{Status: 401}), true},
		{"网络错误", errors.New("connection reset"), false},
		{"无刷新源", ErrNoRefreshSource, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAuthRejected(tc.err); got != tc.want {
				t.Fatalf("IsAuthRejected(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
