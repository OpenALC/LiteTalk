package api

import (
	"fmt"
	"testing"
)

// assertNoEmail 断言 friendView 的 user 子对象不含 email 键。
func assertNoEmail(t *testing.T, item map[string]any) {
	t.Helper()
	u, ok := item["user"].(map[string]any)
	if !ok {
		t.Fatalf("user 字段缺失或类型错误: %+v", item)
	}
	if _, leak := u["email"]; leak {
		t.Fatalf("好友响应泄露 email: %+v", u)
	}
	if u["nickname"] == "" || u["tag"] == "" {
		t.Fatalf("公开视图缺 nickname/tag: %+v", u)
	}
}

// TestFriendsResponseNoEmail 好友相关响应一律 PublicUser 视图：对端 email 不外泄（P1）。
// 昵称#tag 只有 1 万种组合可枚举，带 email 等于公开任意用户邮箱。
func TestFriendsResponseNoEmail(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "pa@ex.com", "隐私A", "password123")
	bTok, _ := registerUser(t, h, mem, "pb@ex.com", "隐私B", "password123")

	// A 加 B（请求响应里的 user 是对端）。
	bu, err := mem.UserByEmail(t.Context(), "pb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("隐私B#%s", bu.Tag)}, 201)
	assertNoEmail(t, fr)

	fid := int(fr["id"].(float64))

	// B 接受（响应对 A 视角）。
	acc := postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	assertNoEmail(t, acc)

	// 双方列表（对端 user）。
	for _, tok := range []string{aTok, bTok} {
		list := getJSON(t, h, "/api/v1/friends", tok, 200).([]any)
		if len(list) != 1 {
			t.Fatalf("好友列表应有 1 条，实际 %d", len(list))
		}
		assertNoEmail(t, list[0].(map[string]any))
	}

	// PATCH 备注响应对端也无 email。
	patched := patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", fid), aTok,
		map[string]string{"remark": "同事"}, 200)
	assertNoEmail(t, patched)
}

// TestSelfResponseKeepsEmail 本人 /me 与注册自响应仍带 email（视图只约束「他人」）。
func TestSelfResponseKeepsEmail(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	access, _ := registerUser(t, h, mem, "self@ex.com", "本人视图", "password123")
	me := getJSON(t, h, "/api/v1/users/me", access, 200).(map[string]any)
	if me["email"] != "self@ex.com" {
		t.Fatalf("/me 应保留本人 email，实际 %v", me["email"])
	}
}

// TestPatchFriendRemark 备注/分组：发起方可改并随列表返回，被请求方 403。
func TestPatchFriendRemark(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "fa@ex.com", "好友A", "password123")
	bTok, _ := registerUser(t, h, mem, "fb@ex.com", "好友B", "password123")

	// A 加 B（tag 随机生成，从 mem 取真实值拼 昵称#tag）。
	bu, err := mem.UserByEmail(t.Context(), "fb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	req := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("好友B#%s", bu.Tag)}, 201)
	fid := req["id"].(float64)
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", int(fid)), bTok, nil, 200)

	// A（发起方）设置备注与分组。
	patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", int(fid)), aTok,
		map[string]string{"remark": "同事小王", "group": "同事"}, 200)

	// A 列表带出备注/分组。
	list := getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	if len(list) != 1 {
		t.Fatalf("A 好友数 = %d", len(list))
	}
	v := list[0].(map[string]any)
	if v["remark"] != "同事小王" || v["group"] != "同事" {
		t.Fatalf("备注/分组未带回: %+v", v)
	}

	// B（被请求方）改同一条行 → 403（备注是发起方视角）。
	patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", int(fid)), bTok,
		map[string]string{"remark": "x"}, 403)

	// 超长备注 → 400。
	patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", int(fid)), aTok,
		map[string]string{"remark": string(make([]byte, 65))}, 400)
}

// TestCategoriesCRUD 分类创建/列频道/删除：非空未带参 409，with_channels=1 级联。
func TestCategoriesCRUD(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "ca@ex.com", "分类A", "password123")
	bTok, _ := registerUser(t, h, mem, "cb@ex.com", "分类B", "password123")

	// A 建服务器。
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "测试服"}, 201)
	sid := int(sv["id"].(float64))
	// B 经邀请加入（成员可读频道）。
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok, map[string]any{}, 201)
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": inv["code"].(string)}, 200)

	// 创建分类。
	cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), aTok,
		map[string]string{"name": "语音分组"}, 201)
	catID := int(cat["id"].(float64))

	// 在分类下建频道（先建频道再 PATCH 归属，createChannelReq 无 category 字段）。
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok,
		map[string]any{"name": "开黑房", "type": "voice"}, 201)
	chID := int(ch["id"].(float64))
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d", chID), aTok,
		map[string]any{"category_id": catID}, 200)

	// GET channels 返回 categories + 频道带 category_id。
	out := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), bTok, 200).(map[string]any)
	cats := out["categories"].([]any)
	chans := out["channels"].([]any)
	if len(cats) != 1 || cats[0].(map[string]any)["name"] != "语音分组" {
		t.Fatalf("categories = %+v", cats)
	}
	found := false
	for _, c := range chans {
		m := c.(map[string]any)
		if m["category_id"] != nil && int(m["category_id"].(float64)) == catID {
			found = true
		}
	}
	if !found {
		t.Fatalf("频道未带 category_id: %+v", chans)
	}

	// 非空未带参删除 → 409。
	delJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catID), aTok, 409)
	// with_channels=1 级联删除。
	delJSON(t, h, fmt.Sprintf("/api/v1/categories/%d?with_channels=1", catID), aTok, 200)
	// 频道一并消失。
	out = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok, 200).(map[string]any)
	if len(out["channels"].([]any)) != 0 || len(out["categories"].([]any)) != 0 {
		t.Fatalf("级联删除后仍有残留: %+v", out)
	}
}

// TestCategoryForbidden 非 owner/admin 创建分类 → 403。
func TestCategoryForbidden(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "da@ex.com", "权限A", "password123")
	bTok, _ := registerUser(t, h, mem, "db@ex.com", "权限B", "password123")
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "权限服"}, 201)
	sid := int(sv["id"].(float64))
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok, map[string]any{}, 201)
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": inv["code"].(string)}, 200)

	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), bTok,
		map[string]string{"name": "越权"}, 403)
}
