package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #38 B2：私聊/群私聊 REST、成员管理与消息鉴权。
// 夹具直接用注册/好友 API 造用户（真实路径），批量好友才直写 Store（数量大）。

// convWorld 一套会话夹具：alice 与 bob/carol/dave 是好友，erin 不是。
type convWorld struct {
	t     *testing.T
	srv   *Server
	h     http.Handler
	mem   *store.Mem
	alice string
	bob   string
	carol string
	dave  string
	erin  string
	// ids 与上面令牌同序。
	aliceID, bobID, carolID, daveID, erinID int64
}

func newConvWorld(t *testing.T) *convWorld {
	t.Helper()
	srv, mem := newTestServer(t)
	h := srv.Handler()
	w := &convWorld{
		t: t, srv: srv, h: h, mem: mem,
		alice: mustReg(t, h, mem, "cv-a@ex.com", "cvAlice"),
		bob:   mustReg(t, h, mem, "cv-b@ex.com", "cvBob"),
		carol: mustReg(t, h, mem, "cv-c@ex.com", "cvCarol"),
		dave:  mustReg(t, h, mem, "cv-d@ex.com", "cvDave"),
		erin:  mustReg(t, h, mem, "cv-e@ex.com", "cvErin"),
	}
	w.aliceID = meID(t, h, w.alice)
	w.bobID = meID(t, h, w.bob)
	w.carolID = meID(t, h, w.carol)
	w.daveID = meID(t, h, w.dave)
	w.erinID = meID(t, h, w.erin)
	// alice 与 bob/carol/dave 已接受好友；erin 不是任何人的好友。
	w.befriend(w.alice, w.bob)
	w.befriend(w.alice, w.carol)
	w.befriend(w.alice, w.dave)
	return w
}

// befriend 走真实好友请求/接受路径。
func (w *convWorld) befriend(from, to string) {
	w.t.Helper()
	me := getJSON(w.t, w.h, "/api/v1/users/me", to, http.StatusOK).(map[string]any)
	target := me["nickname"].(string) + "#" + me["tag"].(string)
	req := postJSON(w.t, w.h, "/api/v1/friends/requests", from,
		map[string]string{"nickname": target}, http.StatusCreated)
	postJSON(w.t, w.h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", asID(req["id"])),
		to, nil, http.StatusOK)
}

// uidList 把用户 ID 列表转成请求体里的字符串数组（ID wire 口径）。
func uidList(ids ...int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// memberRoles 会话响应 members → uid → role，并断言成员载荷不含 email。
func memberRoles(t *testing.T, resp map[string]any) map[int64]string {
	t.Helper()
	arr, ok := resp["members"].([]any)
	if !ok {
		t.Fatalf("members 不是数组: %v", resp)
	}
	out := make(map[int64]string, len(arr))
	for _, raw := range arr {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("成员不是对象: %v", raw)
		}
		if _, leaked := m["email"]; leaked {
			t.Fatalf("成员载荷泄露 email: %v", m)
		}
		out[asID(m["user_id"])] = m["role"].(string)
	}
	return out
}

// listConversations 取会话数组（校验空结果也是数组）。
func listConversations(t *testing.T, h http.Handler, token string) []any {
	t.Helper()
	body := getJSON(t, h, "/api/v1/conversations", token, http.StatusOK)
	arr, ok := body.([]any)
	if !ok {
		t.Fatalf("会话列表不是数组: %v", body)
	}
	return arr
}

// TestConversationDirectLifecycle direct 创建/幂等/非好友/消息鉴权/越权端点/删除。
func TestConversationDirectLifecycle(t *testing.T) {
	w := newConvWorld(t)
	h := w.h

	created := postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.bobID)}, http.StatusCreated)
	convID := asID(created["id"])
	if convID == 0 || created["kind"] != "direct" || created["name"] != "" {
		t.Fatalf("direct 响应形状不符: %v", created)
	}
	roles := memberRoles(t, created)
	if len(roles) != 2 || roles[w.aliceID] != "member" || roles[w.bobID] != "member" {
		t.Fatalf("direct 成员角色不符: %v", roles)
	}

	// 反向重复创建：幂等 200 + 同一会话 ID，不产生第二行。
	again := postJSON(t, h, "/api/v1/conversations", w.bob,
		map[string]any{"kind": "direct", "user_ids": uidList(w.aliceID)}, http.StatusOK)
	if asID(again["id"]) != convID {
		t.Fatalf("幂等创建返回不同会话: %v vs %d", again, convID)
	}

	// 非好友 403 not_friend；创建者自己/多人/bad kind 400。
	nf := postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.erinID)}, http.StatusForbidden)
	if nf["code"] != "not_friend" {
		t.Fatalf("非好友 code = %v", nf["code"])
	}
	for name, body := range map[string]map[string]any{
		"包含自己":      {"kind": "direct", "user_ids": uidList(w.aliceID)},
		"direct 多人": {"kind": "direct", "user_ids": uidList(w.bobID, w.carolID)},
		"kind 非法":   {"kind": "channel", "user_ids": uidList(w.bobID)},
	} {
		resp := postJSON(t, h, "/api/v1/conversations", w.alice, body, http.StatusBadRequest)
		want := "bad_members"
		if name == "kind 非法" {
			want = "bad_kind"
		}
		if resp["code"] != want {
			t.Fatalf("%s code = %v，期望 %s", name, resp["code"], want)
		}
	}

	// 参与者可读，非参与者与不存在一样 404。
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice, http.StatusOK)
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.bob, http.StatusOK)
	for _, path := range []string{fmt.Sprintf("/api/v1/conversations/%d", convID), "/api/v1/conversations/999999"} {
		resp := getJSON(t, h, path, w.erin, http.StatusNotFound).(map[string]any)
		if resp["code"] != "not_found" {
			t.Fatalf("非参与者 %s code = %v", path, resp["code"])
		}
	}

	// 列表：双方各 1 条，非成员空数组。
	if got := listConversations(t, h, w.alice); len(got) != 1 || asID(got[0].(map[string]any)["id"]) != convID {
		t.Fatalf("alice 会话列表不符: %v", got)
	}
	if got := listConversations(t, h, w.bob); len(got) != 1 {
		t.Fatalf("bob 会话列表不符: %v", got)
	}
	if got := listConversations(t, h, w.erin); len(got) != 0 {
		t.Fatalf("非成员会话列表应为空: %v", got)
	}

	// DM 消息：成员可发可读；非成员 404（不泄露会话存在）。
	sendMsg(t, h, w.alice, convID, "私聊消息")
	msgs := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.bob, http.StatusOK).([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "私聊消息" {
		t.Fatalf("DM 消息列表不符: %v", msgs)
	}
	nfMsg := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.erin,
		map[string]string{"content": "偷看"}, http.StatusNotFound)
	if nfMsg["code"] != "not_found" {
		t.Fatalf("非成员发消息 code = %v", nfMsg["code"])
	}
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.erin, http.StatusNotFound)

	// 服务器频道管理端点不能作用于 DM：PATCH/DELETE/覆盖/闭麦一律 404。
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", convID), w.alice,
		map[string]string{"name": "偷改"}, http.StatusNotFound)
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d", convID), w.alice, nil, http.StatusNotFound)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/overwrites", convID), w.alice, http.StatusNotFound)
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", convID, w.bobID),
		w.alice, map[string]bool{"muted": true}, http.StatusNotFound)
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice, http.StatusOK)

	// direct 的管理端点：改名/加人/移除成员都是 400 not_group。
	notGroup := patchJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice,
		map[string]string{"name": "私聊名"}, http.StatusBadRequest)
	if notGroup["code"] != "not_group" {
		t.Fatalf("direct 改名 code = %v", notGroup["code"])
	}
	notGroup = postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", convID), w.alice,
		map[string]any{"user_ids": uidList(w.daveID)}, http.StatusBadRequest)
	if notGroup["code"] != "not_group" {
		t.Fatalf("direct 加人 code = %v", notGroup["code"])
	}
	notGroup = delJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members/%d", convID, w.bobID),
		w.alice, http.StatusBadRequest)
	if notGroup["code"] != "not_group" {
		t.Fatalf("direct 移除成员 code = %v", notGroup["code"])
	}

	// 任一方可删；硬删消息与成员，双方都看不到。
	delJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.bob, http.StatusNoContent)
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice, http.StatusNotFound)
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.bob, http.StatusNotFound)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.alice, http.StatusNotFound)
	if msgs, err := w.mem.MessagesBefore(context.Background(), convID, 0, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("删除会话应级联消息: %v %v", msgs, err)
	}
	for _, tok := range []string{w.alice, w.bob} {
		if got := listConversations(t, h, tok); len(got) != 0 {
			t.Fatalf("删除后会话列表应为空: %v", got)
		}
	}
}

// TestConversationGroupLifecycle 群聊创建/改名/加人/退出/owner 转让/解散。
func TestConversationGroupLifecycle(t *testing.T) {
	w := newConvWorld(t)
	h := w.h

	created := postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "group", "name": "开黑", "user_ids": uidList(w.bobID, w.carolID)},
		http.StatusCreated)
	convID := asID(created["id"])
	if created["kind"] != "group" || created["name"] != "开黑" {
		t.Fatalf("group 响应形状不符: %v", created)
	}
	roles := memberRoles(t, created)
	if len(roles) != 3 || roles[w.aliceID] != "owner" ||
		roles[w.bobID] != "member" || roles[w.carolID] != "member" {
		t.Fatalf("group 成员角色不符: %v", roles)
	}

	// 创建参数校验：人数、名字、重复成员、非好友。
	for name, c := range map[string]struct {
		body map[string]any
		code string
	}{
		"仅 1 人":  {map[string]any{"kind": "group", "user_ids": uidList(w.bobID)}, "bad_members"},
		"50 人邀请": {map[string]any{"kind": "group", "user_ids": manyIDs(50)}, "bad_members"},
		"名字过长":   {map[string]any{"kind": "group", "name": strings.Repeat("名", 65), "user_ids": uidList(w.bobID, w.carolID)}, "bad_name"},
		"成员重复":   {map[string]any{"kind": "group", "user_ids": uidList(w.bobID, w.bobID)}, "bad_members"},
		"含非好友":   {map[string]any{"kind": "group", "user_ids": uidList(w.bobID, w.erinID)}, "not_friend"},
	} {
		status := http.StatusBadRequest
		if c.code == "not_friend" {
			status = http.StatusForbidden
		}
		resp := postJSON(t, h, "/api/v1/conversations", w.alice, c.body, status)
		if resp["code"] != c.code {
			t.Fatalf("%s code = %v，期望 %s", name, resp["code"], c.code)
		}
	}

	// 非 owner 不能改名/加人/移除他人。
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, fmt.Sprintf("/api/v1/conversations/%d", convID), map[string]string{"name": "bob改"}},
		{http.MethodPost, fmt.Sprintf("/api/v1/conversations/%d/members", convID), map[string]any{"user_ids": uidList(w.daveID)}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/conversations/%d/members/%d", convID, w.aliceID), nil},
	} {
		resp := doJSON(t, h, c.method, c.path, w.bob, c.body, http.StatusForbidden)
		if resp["code"] != "forbidden" {
			t.Fatalf("%s %s code = %v", c.method, c.path, resp["code"])
		}
	}

	// owner 改名；加已接受好友成功，加非好友 403，重复成员 400。
	renamed := patchJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice,
		map[string]string{"name": "夜车"}, http.StatusOK)
	if renamed["name"] != "夜车" {
		t.Fatalf("改名未生效: %v", renamed)
	}
	added := postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", convID), w.alice,
		map[string]any{"user_ids": uidList(w.daveID)}, http.StatusOK)
	if roles := memberRoles(t, added); len(roles) != 4 || roles[w.daveID] != "member" {
		t.Fatalf("加人后成员不符: %v", roles)
	}
	nf := postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", convID), w.alice,
		map[string]any{"user_ids": uidList(w.erinID)}, http.StatusForbidden)
	if nf["code"] != "not_friend" {
		t.Fatalf("加非好友 code = %v", nf["code"])
	}
	dup := postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", convID), w.alice,
		map[string]any{"user_ids": uidList(w.bobID)}, http.StatusBadRequest)
	if dup["code"] != "bad_members" {
		t.Fatalf("重复加人 code = %v", dup["code"])
	}

	// owner 移除 bob；bob 随后 404。
	removed := delJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members/%d", convID, w.bobID),
		w.alice, http.StatusOK)
	if roles := memberRoles(t, removed); len(roles) != 3 || roles[w.bobID] != "" {
		t.Fatalf("移除后成员不符: %v", roles)
	}
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.bob, http.StatusNotFound)

	// 普通成员 carol 自我退出（leave）。
	left := delJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members/%d", convID, w.carolID),
		w.carol, http.StatusOK)
	if roles := memberRoles(t, left); len(roles) != 2 || roles[w.carolID] != "" {
		t.Fatalf("退出后成员不符: %v", roles)
	}
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.carol, http.StatusNotFound)

	// owner alice 退出：owner 转给剩余成员 dave。
	afterLeave := delJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members/%d", convID, w.aliceID),
		w.alice, http.StatusOK)
	if roles := memberRoles(t, afterLeave); len(roles) != 1 || roles[w.daveID] != "owner" {
		t.Fatalf("owner 转让不符: %v", roles)
	}
	// dave 现在是 owner，可正常读取；alice 已非成员 404。
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.dave, http.StatusOK)
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice, http.StatusNotFound)

	// 最后一人退出：会话删除 204。
	delJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members/%d", convID, w.daveID),
		w.dave, http.StatusNoContent)
	getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.dave, http.StatusNotFound)
	if members, err := w.mem.ChannelMembers(context.Background(), convID); err != nil || len(members) != 0 {
		t.Fatalf("会话删除应级联成员: %v %v", members, err)
	}
}

// TestConversationMemberCapAndRateLimit 群聊 50 人上限 + 创建频控 429。
func TestConversationMemberCapAndRateLimit(t *testing.T) {
	w := newConvWorld(t)
	h := w.h

	created := postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "group", "user_ids": uidList(w.bobID, w.carolID)}, http.StatusCreated)
	convID := asID(created["id"])

	// 直写批量好友（真实 API 造 47 个好友太慢，数量本身不是被测点）。
	bulk := seedFriends(t, w.mem, w.aliceID, 48)
	if resp := postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", convID), w.alice,
		map[string]any{"user_ids": uidList(bulk[:47]...)}, http.StatusOK); resp == nil {
		t.Fatal("批量加人失败")
	}
	if roles := memberRoles(t, getJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d", convID), w.alice,
		http.StatusOK).(map[string]any)); len(roles) != 50 {
		t.Fatalf("加满后成员数 = %d，期望 50", len(roles))
	}
	over := postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", convID), w.alice,
		map[string]any{"user_ids": uidList(bulk[47])}, http.StatusBadRequest)
	if over["code"] != "too_many_members" {
		t.Fatalf("超上限 code = %v", over["code"])
	}

	// 创建频控 10 次/分/用户：新用户 11 次直聊，第 11 次 429。
	rateTok := mustReg(t, h, w.mem, "cv-rate@ex.com", "cvRate")
	rateID := meID(t, h, rateTok)
	friends := seedFriends(t, w.mem, rateID, 11)
	for i := 0; i < 10; i++ {
		postJSON(t, h, "/api/v1/conversations", rateTok,
			map[string]any{"kind": "direct", "user_ids": uidList(friends[i])}, http.StatusCreated)
	}
	limited := postJSON(t, h, "/api/v1/conversations", rateTok,
		map[string]any{"kind": "direct", "user_ids": uidList(friends[10])}, http.StatusTooManyRequests)
	if limited["code"] != "rate_limited" {
		t.Fatalf("频控 code = %v", limited["code"])
	}
}

// seedFriends 直写 n 个新用户并与 owner 建立已接受好友关系，返回用户 ID。
func seedFriends(t *testing.T, mem *store.Mem, ownerID int64, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		u := &store.User{
			Email:        fmt.Sprintf("cv-seed-%d-%d@ex.com", ownerID, i),
			Nickname:     fmt.Sprintf("cvSeed%d-%d", ownerID%100000, i),
			Tag:          "0001",
			PasswordHash: "h",
		}
		if err := mem.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser(seed %d): %v", i, err)
		}
		if err := mem.CreateFriendship(ctx, &store.Friendship{
			UserID: ownerID, FriendID: u.ID, Status: store.FriendAccepted,
		}); err != nil {
			t.Fatalf("CreateFriendship(seed %d): %v", i, err)
		}
		out = append(out, u.ID)
	}
	return out
}

// manyIDs 生成 n 个互不相同、必然不存在的字符串 ID（参数校验用）。
func manyIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, strconv.FormatInt(int64(9_000_000_000+i), 10))
	}
	return out
}
