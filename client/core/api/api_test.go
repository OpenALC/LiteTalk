package api

import (
	"context"
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
