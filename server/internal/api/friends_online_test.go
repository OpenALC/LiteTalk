package api

import (
	"fmt"
	"testing"
)

// presenceKeyOf 与 view.go / signal.presenceKey 的键格式一致（手工同步）。
func presenceKeyOf(uid int64) string {
	return fmt.Sprintf("presence:%d", uid)
}

// TestFriendsOnlineField 好友响应 user.online 随 presence:{uid} 变化（#11）：
// 键存在=true，删除后=false；键名恒输出（不因 false 省略）。
func TestFriendsOnlineField(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "oa@ex.com", "在线A", "password123")
	bTok, _ := registerUser(t, h, mem, "ob@ex.com", "在线B", "password123")
	bu, err := mem.UserByEmail(t.Context(), "ob@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}

	// A 加 B：请求响应里对端 user 的 online 字段恒输出，未上线时 false。
	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("在线B#%s", bu.Tag)}, 201)
	if u := fr["user"].(map[string]any); u["online"] != false {
		t.Fatalf("未上线应 online=false，实际 %v", u["online"])
	}

	// B 接受：响应里的 user 是发起方 A——先让 A 上线，断言 accept 视角的 online。
	au, err := mem.UserByEmail(t.Context(), "oa@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail A: %v", err)
	}
	if err := mem.Set(t.Context(), presenceKeyOf(au.ID), "1", 0); err != nil {
		t.Fatalf("Set presence A: %v", err)
	}
	fid := int(fr["id"].(float64))
	acc := postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	if u := acc["user"].(map[string]any); u["online"] != true {
		t.Fatalf("presence 键存在应 online=true，实际 %v", u["online"])
	}

	// A 的好友列表：对端是 B——B 上线后 true。
	if err := mem.Set(t.Context(), presenceKeyOf(bu.ID), "1", 0); err != nil {
		t.Fatalf("Set presence B: %v", err)
	}
	list := getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	if len(list) != 1 {
		t.Fatalf("好友列表应 1 条，实际 %d", len(list))
	}
	if u := list[0].(map[string]any)["user"].(map[string]any); u["online"] != true {
		t.Fatalf("列表应 online=true，实际 %v", u["online"])
	}

	// B 下线（DEL）：列表翻 false，字段仍输出。
	if err := mem.Del(t.Context(), presenceKeyOf(bu.ID)); err != nil {
		t.Fatalf("Del presence: %v", err)
	}
	list = getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	u := list[0].(map[string]any)["user"].(map[string]any)
	if _, ok := u["online"]; !ok {
		t.Fatalf("online 字段应恒输出: %+v", u)
	}
	if u["online"] != false {
		t.Fatalf("DEL 后应 online=false，实际 %v", u["online"])
	}
}

// TestFriendsOnlineNoKV 无 Presence 依赖时恒离线且不 panic（降级路径）。
func TestFriendsOnlineNoKV(t *testing.T) {
	srv, mem := newTestServer(t)
	srv.presence = nil
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "na@ex.com", "无KVA", "password123")
	bTok, _ := registerUser(t, h, mem, "nb@ex.com", "无KVB", "password123")
	bu, err := mem.UserByEmail(t.Context(), "nb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("无KVB#%s", bu.Tag)}, 201)
	fid := int(fr["id"].(float64))
	acc := postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	if u := acc["user"].(map[string]any); u["online"] != false {
		t.Fatalf("无 KV 时应恒 false，实际 %v", u["online"])
	}
}
