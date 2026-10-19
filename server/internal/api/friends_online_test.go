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
	fid := int(asID(fr["id"]))
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
	fid := int(asID(fr["id"]))
	acc := postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	if u := acc["user"].(map[string]any); u["online"] != false {
		t.Fatalf("无 KV 时应恒 false，实际 %v", u["online"])
	}
}

// TestMembersOnlineField 成员列表带在线态（#11 验收项）：在线成员 true、
// 离线成员 false，字段恒输出且不含 email。
func TestMembersOnlineField(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "ma@ex.com", "成员甲", "password123")
	bTok, _ := registerUser(t, h, mem, "mb@ex.com", "成员乙", "password123")
	bu, err := mem.UserByEmail(t.Context(), "mb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}

	// A 建服，B 经邀请加入。
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "在线服"}, 201)
	sid := asID(sv["id"])
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok, map[string]any{}, 201)
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": inv["code"].(string)}, 200)

	// B 在线（A 不写 presence → 离线）。
	if err := mem.Set(t.Context(), presenceKeyOf(bu.ID), "1", 0); err != nil {
		t.Fatalf("Set presence B: %v", err)
	}
	list := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/members", sid), aTok, 200).([]any)
	if len(list) != 2 {
		t.Fatalf("成员应 2 人，实际 %d", len(list))
	}
	for _, raw := range list {
		m := raw.(map[string]any)
		if _, leak := m["email"]; leak {
			t.Fatalf("成员视图泄露 email: %+v", m)
		}
		online, ok := m["online"].(bool)
		if !ok {
			t.Fatalf("online 字段应恒输出: %+v", m)
		}
		want := asID(m["user_id"]) == bu.ID
		if online != want {
			t.Fatalf("成员 %v online=%v，期望 %v（仅 B 在线）", m["user_id"], online, want)
		}
	}
}

// TestPendingFriendHidesOnline pending 关系不暴露在线态（#11 P2 隐私）：
// 单向发出请求即可探测在线与否是不允许的；接受后才返回真实值（含 /sync）。
func TestPendingFriendHidesOnline(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "pfa@ex.com", "待处理甲", "password123")
	bTok, _ := registerUser(t, h, mem, "pfb@ex.com", "待处理乙", "password123")
	bu, err := mem.UserByEmail(t.Context(), "pfb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	// B 已在线，但关系还是 pending。
	if err := mem.Set(t.Context(), presenceKeyOf(bu.ID), "1", 0); err != nil {
		t.Fatalf("Set presence B: %v", err)
	}

	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("待处理乙#%s", bu.Tag)}, 201)
	if u := fr["user"].(map[string]any); u["online"] != false {
		t.Fatalf("pending 请求响应不应返回在线态，实际 %v", u["online"])
	}

	// A 的好友列表与 /sync 里，pending 对端 online 恒 false。
	list := getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	if len(list) != 1 {
		t.Fatalf("好友关系应 1 条，实际 %d", len(list))
	}
	if u := list[0].(map[string]any)["user"].(map[string]any); u["online"] != false {
		t.Fatalf("pending 关系的列表视图不应返回在线态，实际 %v", u["online"])
	}
	syncBody := getJSON(t, h, "/api/v1/sync", aTok, 200).(map[string]any)
	syncFriends := syncBody["friends"].([]any)
	if len(syncFriends) != 1 {
		t.Fatalf("sync 好友应 1 条，实际 %d", len(syncFriends))
	}
	if u := syncFriends[0].(map[string]any)["user"].(map[string]any); u["online"] != false {
		t.Fatalf("pending 关系在 /sync 中不应返回在线态，实际 %v", u["online"])
	}

	// 接受后才是好友：online 恢复真实值。
	fid := asID(fr["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	list = getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	if u := list[0].(map[string]any)["user"].(map[string]any); u["online"] != true {
		t.Fatalf("accepted 关系应返回真实在线态 true，实际 %v", u["online"])
	}
}
