package state

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	r, err := c.Register("游客-2733", "x@y.com", "password123", "")
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
