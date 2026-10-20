package api

import (
	"context"
	"net/http"
	"testing"
)

// TestFriendRequestNicknameCaseSensitive #24 P2：昵称统一为大小写敏感后，
// 同名不同大小写 + 同 tag 是两个账号；按「昵称#tag」精确匹配，错误文案不变。
func TestFriendRequestNicknameCaseSensitive(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	requester, _ := registerUser(t, h, mem, "case-req@ex.com", "请求者", "password123")
	upperTok, _ := registerUser(t, h, mem, "case-b@ex.com", "Alice", "password123")
	_, _ = registerUser(t, h, mem, "case-c@ex.com", "alice", "password123")

	// 注册 tag 由服务端随机，这里把 alice 的 tag 改成与 Alice 相同，
	// 构造「同名不同大小写 + 同 tag」的账号对（大小写敏感语义下允许）。
	tag := userTag(t, h, upperTok)
	lower, err := mem.UserByEmail(ctx, "case-c@ex.com")
	if err != nil {
		t.Fatalf("取 alice 账号失败: %v", err)
	}
	lower.Tag = tag
	if err := mem.UpdateUser(ctx, lower); err != nil {
		t.Fatalf("改写 alice 的 tag 失败: %v", err)
	}
	upper, err := mem.UserByEmail(ctx, "case-b@ex.com")
	if err != nil {
		t.Fatalf("取 Alice 账号失败: %v", err)
	}

	// 精确大小写各自命中对应账号。
	frUpper := postJSON(t, h, "/api/v1/friends/requests", requester, map[string]string{
		"nickname": "Alice#" + tag,
	}, http.StatusCreated)
	assertFriendTarget(t, frUpper, upper.ID, "Alice")

	frLower := postJSON(t, h, "/api/v1/friends/requests", requester, map[string]string{
		"nickname": "alice#" + tag,
	}, http.StatusCreated)
	assertFriendTarget(t, frLower, lower.ID, "alice")

	// 既非 Alice 也非 alice 的第三种大小写不存在：404，文案不回归。
	missing := postJSON(t, h, "/api/v1/friends/requests", requester, map[string]string{
		"nickname": "ALICE#" + tag,
	}, http.StatusNotFound)
	if missing["code"] != "not_found" || missing["detail"] != "用户不存在" || missing["message"] != "用户不存在" {
		t.Fatalf("目标不存在响应变化: %v", missing)
	}

	// 重复添加精确昵称仍按既有约定 409，文案不回归。
	again := postJSON(t, h, "/api/v1/friends/requests", requester, map[string]string{
		"nickname": "Alice#" + tag,
	}, http.StatusConflict)
	if again["code"] != "request_exists" || again["detail"] != "已存在好友请求" {
		t.Fatalf("重复好友请求响应变化: %v", again)
	}
}

// assertFriendTarget 好友请求响应里的目标用户应是预期账号。
func assertFriendTarget(t *testing.T, resp map[string]any, wantID int64, wantNickname string) {
	t.Helper()
	u, ok := resp["user"].(map[string]any)
	if !ok {
		t.Fatalf("好友请求响应缺 user: %v", resp)
	}
	if got := asID(u["id"]); got != wantID {
		t.Fatalf("好友目标 id=%d，期望 %d（昵称 %q）", got, wantID, wantNickname)
	}
	if got, _ := u["nickname"].(string); got != wantNickname {
		t.Fatalf("好友目标昵称=%q，期望 %q", got, wantNickname)
	}
}
