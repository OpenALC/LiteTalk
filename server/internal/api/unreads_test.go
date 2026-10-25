package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #40 B2：未读/提及计数 REST 快照、已读水位与编辑/删除后的计数广播。

// unreadSnapshot 解析 GET /unreads 并按 ID 建索引；同时保留响应顺序用于断言升序。
type unreadSnapshot struct {
	channels   map[int64]store.Unread
	channelIDs []int64
	servers    map[int64]store.Unread
	serverIDs  []int64
}

func fetchUnreads(t *testing.T, h http.Handler, token string) unreadSnapshot {
	t.Helper()
	body := getJSON(t, h, "/api/v1/unreads", token, http.StatusOK)
	obj, ok := body.(map[string]any)
	if !ok {
		t.Fatalf("unreads 响应不是对象: %v", body)
	}
	out := unreadSnapshot{
		channels:   map[int64]store.Unread{},
		channelIDs: []int64{},
		servers:    map[int64]store.Unread{},
		serverIDs:  []int64{},
	}
	rawChannels, ok := obj["channels"].([]any)
	if !ok || rawChannels == nil {
		t.Fatalf("channels 应为非 null 数组: %v", obj["channels"])
	}
	for _, raw := range rawChannels {
		ch, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("channels 元素不是对象: %v", raw)
		}
		id := asID(ch["channel_id"])
		out.channelIDs = append(out.channelIDs, id)
		out.channels[id] = store.Unread{
			UnreadCount:  int(ch["unread_count"].(float64)),
			MentionCount: int(ch["mention_count"].(float64)),
		}
	}
	rawServers, ok := obj["servers"].([]any)
	if !ok || rawServers == nil {
		t.Fatalf("servers 应为非 null 数组: %v", obj["servers"])
	}
	for _, raw := range rawServers {
		srv, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("servers 元素不是对象: %v", raw)
		}
		id := asID(srv["server_id"])
		out.serverIDs = append(out.serverIDs, id)
		out.servers[id] = store.Unread{
			UnreadCount:  int(srv["unread_count"].(float64)),
			MentionCount: int(srv["mention_count"].(float64)),
		}
	}
	return out
}

func assertUnread(t *testing.T, got store.Unread, wantUnread, wantMentions int) {
	t.Helper()
	if got.UnreadCount != wantUnread || got.MentionCount != wantMentions {
		t.Fatalf("计数 = %+v，期望 unread=%d mention=%d", got, wantUnread, wantMentions)
	}
}

func assertIDSorted(t *testing.T, ids []int64) {
	t.Helper()
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Fatalf("ID 未按升序输出: %v", ids)
	}
}

// unreadTestServer 创建服务器并返回 ID。
func unreadTestServer(t *testing.T, h http.Handler, token, name string) int64 {
	t.Helper()
	return asID(postJSON(t, h, "/api/v1/servers", token,
		map[string]string{"name": name, "icon_name": "music", "icon_color": "#4FA4F3"},
		http.StatusCreated)["id"])
}

// unreadTestChannel 在服务器下创建频道并返回 ID。
func unreadTestChannel(t *testing.T, h http.Handler, token string, serverID int64, name, typ string) int64 {
	t.Helper()
	return asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), token,
		map[string]string{"name": name, "type": typ}, http.StatusCreated)["id"])
}

// unreadTestJoin 用邀请码把 member 加入服务器。
func unreadTestJoin(t *testing.T, h http.Handler, owner, member string, serverID int64) {
	t.Helper()
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), owner,
		map[string]any{}, http.StatusCreated)
	postJSON(t, h, "/api/v1/servers/join", member,
		map[string]string{"code": inv["code"].(string)}, http.StatusOK)
}

// TestListUnreadsSnapshot 快照包含 0 值项、跨服聚合、排除自己/软删消息、
// 提及计数正确，数组按 ID 升序且空用户返回 []。
func TestListUnreadsSnapshot(t *testing.T) {
	w := newConvWorld(t)
	h := w.h

	serverA := unreadTestServer(t, h, w.alice, "未读甲服")
	serverB := unreadTestServer(t, h, w.alice, "未读乙服")
	aText := unreadTestChannel(t, h, w.alice, serverA, "文字甲", "text")
	aZero := unreadTestChannel(t, h, w.alice, serverA, "零值", "text")
	aVoice := unreadTestChannel(t, h, w.alice, serverA, "语音", "voice")
	bText := unreadTestChannel(t, h, w.alice, serverB, "文字乙", "text")
	unreadTestJoin(t, h, w.alice, w.bob, serverA)
	unreadTestJoin(t, h, w.alice, w.bob, serverB)

	convID := asID(postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.bobID)}, http.StatusCreated)["id"])

	// 软删消息不计数：先发、提及 alice，再删除。
	deleted := asID(postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", aText), w.bob,
		map[string]any{"content": "将被删除", "mentions": uidList(w.aliceID)}, http.StatusCreated)["id"])
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", aText, deleted), w.bob, http.StatusNoContent)

	msgA1 := asID(postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", aText), w.bob,
		map[string]any{"content": "甲一提及", "mentions": uidList(w.aliceID)}, http.StatusCreated)["id"])
	msgA2 := sendMsg(t, h, w.bob, aText, "甲二")
	msgOwn := sendMsg(t, h, w.alice, aText, "我自己的消息")
	msgB := asID(postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", bText), w.bob,
		map[string]any{"content": "乙服提及", "mentions": uidList(w.aliceID)}, http.StatusCreated)["id"])
	msgD := asID(postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.bob,
		map[string]any{"content": "私聊提及", "mentions": uidList(w.aliceID)}, http.StatusCreated)["id"])
	if msgA1 >= msgA2 || msgA2 >= msgOwn || msgA1 >= msgOwn || msgB == 0 || msgD == 0 {
		t.Fatalf("消息 ID 不满足 Snowflake 递增假设: %d %d %d", msgA1, msgA2, msgOwn)
	}

	snap := fetchUnreads(t, h, w.alice)
	assertUnread(t, snap.channels[aText], 2, 1) // 排除自己的消息与软删消息
	assertUnread(t, snap.channels[aZero], 0, 0)
	assertUnread(t, snap.channels[bText], 1, 1)
	assertUnread(t, snap.channels[convID], 1, 1)
	if _, ok := snap.channels[aVoice]; ok {
		t.Fatal("语音频道不应出现在未读快照")
	}
	if len(snap.channels) != 4 {
		t.Fatalf("频道数 = %d，期望 4（两个文字频道 + 零值频道 + 私聊）: %v", len(snap.channels), snap.channelIDs)
	}
	assertIDSorted(t, snap.channelIDs)
	// 服务器聚合只算服务器文字频道，私聊不计入；零值频道不改变合计。
	assertUnread(t, snap.servers[serverA], 2, 1)
	assertUnread(t, snap.servers[serverB], 1, 1)
	if len(snap.servers) != 2 {
		t.Fatalf("服务器数 = %d，期望 2", len(snap.servers))
	}
	assertIDSorted(t, snap.serverIDs)

	// 无服务器、无会话：数组落 [] 而不是 null。
	empty := fetchUnreads(t, h, w.erin)
	if len(empty.channels) != 0 || len(empty.servers) != 0 {
		t.Fatalf("非成员应为空快照: %+v", empty)
	}
	if empty.channelIDs == nil || empty.serverIDs == nil {
		t.Fatal("空数组应解码为非 nil 切片")
	}
}

// TestMarkChannelRead 幂等/只前进、跨频道拒绝、非成员口径与精确计数广播。
func TestMarkChannelRead(t *testing.T) {
	w := newConvWorld(t)
	h := w.h
	serverID := unreadTestServer(t, h, w.alice, "已读服")
	chID := unreadTestChannel(t, h, w.alice, serverID, "大厅", "text")
	otherCh := unreadTestChannel(t, h, w.alice, serverID, "其它", "text")
	unreadTestJoin(t, h, w.alice, w.bob, serverID)
	convID := asID(postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.bobID)}, http.StatusCreated)["id"])

	msg1 := asID(postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.bob,
		map[string]any{"content": "提及", "mentions": uidList(w.aliceID)}, http.StatusCreated)["id"])
	msg2 := sendMsg(t, h, w.bob, chID, "第二条")
	otherMsg := sendMsg(t, h, w.bob, otherCh, "别的频道")
	convMsg := sendMsg(t, h, w.bob, convID, "私聊")

	hub := &fakeHub{}
	w.srv.hub = hub

	// 正常清计数：响应与广播都只包含当前用户的精确计数。
	resp := doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/read", chID), w.alice,
		map[string]string{"last_message_id": strconv.FormatInt(msg2, 10)}, http.StatusOK)
	if asID(resp["channel_id"]) != chID || int(resp["unread_count"].(float64)) != 0 ||
		int(resp["mention_count"].(float64)) != 0 {
		t.Fatalf("mark read 响应不符: %v", resp)
	}
	events := hub.unreadLog()
	if len(events) != 1 {
		t.Fatalf("mark read 应广播 1 次，实际 %d: %+v", len(events), events)
	}
	if events[0].channelID != strconv.FormatInt(chID, 10) || len(events[0].counts) != 1 {
		t.Fatalf("channel.unread 目标不符: %+v", events[0])
	}
	aliceUID := strconv.FormatInt(w.aliceID, 10)
	if u := events[0].counts[aliceUID]; u.UnreadCount != 0 || u.MentionCount != 0 {
		t.Fatalf("channel.unread 计数不符: %+v", events[0].counts)
	}
	if snap := fetchUnreads(t, h, w.alice); snap.channels[chID] != (store.Unread{}) {
		t.Fatalf("标记已读后计数应为 0: %+v", snap.channels[chID])
	}

	// 旧消息不回退：水位仍停在 msg2，响应计数保持 0。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/read", chID), w.alice,
		map[string]string{"last_message_id": strconv.FormatInt(msg1, 10)}, http.StatusOK)
	read, err := w.mem.ReadState(context.Background(), w.aliceID, chID)
	if err != nil || read != msg2 {
		t.Fatalf("水位应保持 %d，实际 %d (%v)", msg2, read, err)
	}
	if got := hub.unreadLog(); len(got) != 2 {
		t.Fatalf("第二次 mark read 应追加广播，实际 %d", len(got))
	}

	// 跨频道消息 400 bad_read，不广播。
	before := len(hub.unreadLog())
	bad := doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/read", chID), w.alice,
		map[string]string{"last_message_id": strconv.FormatInt(otherMsg, 10)}, http.StatusBadRequest)
	if bad["code"] != "bad_read" {
		t.Fatalf("跨频道 code = %v，期望 bad_read", bad["code"])
	}
	if len(hub.unreadLog()) != before {
		t.Fatal("失败的 mark read 不应广播计数")
	}
	// 不存在的消息、0 ID、非法路径一律 400。
	for _, id := range []string{"999999999", "0"} {
		resp := doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/read", chID), w.alice,
			map[string]string{"last_message_id": id}, http.StatusBadRequest)
		if resp["code"] != "bad_read" {
			t.Fatalf("last_message_id=%s code = %v，期望 bad_read", id, resp["code"])
		}
	}
	doJSON(t, h, http.MethodPut, "/api/v1/channels/0/read", w.alice,
		map[string]string{"last_message_id": strconv.FormatInt(msg2, 10)}, http.StatusBadRequest)

	// 非成员：会话 404（不泄露存在性），服务器频道 403；消息存在也不放行。
	notMemberConv := doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/read", convID), w.erin,
		map[string]string{"last_message_id": strconv.FormatInt(convMsg, 10)}, http.StatusNotFound)
	if notMemberConv["code"] != "not_found" {
		t.Fatalf("非会话成员 code = %v，期望 not_found", notMemberConv["code"])
	}
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/read", chID), w.erin,
		map[string]string{"last_message_id": strconv.FormatInt(msg2, 10)}, http.StatusForbidden)
	// 频道不存在 404。
	doJSON(t, h, http.MethodPut, "/api/v1/channels/4242424242/read", w.alice,
		map[string]string{"last_message_id": strconv.FormatInt(msg2, 10)}, http.StatusNotFound)
}

// TestMessageChangeBroadcastsUnread REST 编辑/删除后，同一收件人列表收到更新后的
// channel.unread：编辑改变提及数，删除减少未读数。
func TestMessageChangeBroadcastsUnread(t *testing.T) {
	w := newPermWorld(t)
	hub := &fakeHub{}
	w.srv.hub = hub
	ownerID := meID(t, w.h, w.owner)
	memberID := meID(t, w.h, w.member)

	editCh := unreadTestChannel(t, w.h, w.owner, w.serverID, "编辑计数", "text")
	editMsg := asID(postJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages", editCh), w.member,
		map[string]any{"content": "先提及", "mentions": uidList(ownerID)}, http.StatusCreated)["id"])
	patchJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", editCh, editMsg), w.member,
		map[string]any{"content": "去掉提及", "mentions": []string{}}, http.StatusOK)

	events := hub.unreadLog()
	if len(events) != 1 || events[0].channelID != strconv.FormatInt(editCh, 10) {
		t.Fatalf("编辑后应广播一次 channel.unread: %+v", events)
	}
	ownerKey := strconv.FormatInt(ownerID, 10)
	if u := events[0].counts[ownerKey]; u.UnreadCount != 1 || u.MentionCount != 0 {
		t.Fatalf("编辑后 owner 计数 = %+v，期望 1/0", u)
	}
	memberKey := strconv.FormatInt(memberID, 10)
	if u := events[0].counts[memberKey]; u.UnreadCount != 0 || u.MentionCount != 0 {
		t.Fatalf("编辑后作者自己的计数应为 0: %+v", u)
	}

	delCh := unreadTestChannel(t, w.h, w.owner, w.serverID, "删除计数", "text")
	delMsg := asID(postJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages", delCh), w.member,
		map[string]any{"content": "待删除", "mentions": uidList(ownerID)}, http.StatusCreated)["id"])
	delJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", delCh, delMsg), w.member, http.StatusNoContent)

	events = hub.unreadLog()
	if len(events) != 2 || events[1].channelID != strconv.FormatInt(delCh, 10) {
		t.Fatalf("删除后应追加 channel.unread: %+v", events)
	}
	if u := events[1].counts[ownerKey]; u.UnreadCount != 0 || u.MentionCount != 0 {
		t.Fatalf("删除后 owner 计数 = %+v，期望 0/0", u)
	}
}

// TestReadStatesInitializedOnJoinAndGroupAdd 新成员以加入时的最新消息为水位：
// 历史不显示未读，加入后的新消息正常计数。
func TestReadStatesInitializedOnJoinAndGroupAdd(t *testing.T) {
	w := newConvWorld(t)
	h := w.h

	// 入服：频道里已有历史消息，新成员加入后不应看到全部历史未读。
	serverID := unreadTestServer(t, h, w.alice, "入服水位")
	chID := unreadTestChannel(t, h, w.alice, serverID, "历史", "text")
	history := sendMsg(t, h, w.alice, chID, "加入前的历史")
	unreadTestJoin(t, h, w.alice, w.erin, serverID)
	if got, err := w.mem.ReadState(context.Background(), w.erinID, chID); err != nil || got != history {
		t.Fatalf("入服水位应为最新历史 %d，实际 %d (%v)", history, got, err)
	}
	if snap := fetchUnreads(t, h, w.erin); snap.channels[chID] != (store.Unread{}) {
		t.Fatalf("入服后历史不应计未读: %+v", snap.channels[chID])
	}
	// 加入后的新消息正常计未读。
	sendMsg(t, h, w.alice, chID, "加入后的新消息")
	if snap := fetchUnreads(t, h, w.erin); snap.channels[chID].UnreadCount != 1 {
		t.Fatalf("入服后新消息应为 1 未读: %+v", snap.channels[chID])
	}

	// 群聊加人：历史不计未读，加入后的提及正常计数。
	groupID := asID(postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "group", "name": "水位群", "user_ids": uidList(w.bobID, w.carolID)},
		http.StatusCreated)["id"])
	groupHistory := sendMsg(t, h, w.alice, groupID, "加人前的历史")
	postJSON(t, h, fmt.Sprintf("/api/v1/conversations/%d/members", groupID), w.alice,
		map[string]any{"user_ids": uidList(w.daveID)}, http.StatusOK)
	if got, err := w.mem.ReadState(context.Background(), w.daveID, groupID); err != nil || got != groupHistory {
		t.Fatalf("入群水位应为最新历史 %d，实际 %d (%v)", groupHistory, got, err)
	}
	if snap := fetchUnreads(t, h, w.dave); snap.channels[groupID] != (store.Unread{}) {
		t.Fatalf("入群后历史不应计未读: %+v", snap.channels[groupID])
	}
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", groupID), w.bob,
		map[string]any{"content": "欢迎", "mentions": uidList(w.daveID)}, http.StatusCreated)
	if snap := fetchUnreads(t, h, w.dave); snap.channels[groupID].UnreadCount != 1 ||
		snap.channels[groupID].MentionCount != 1 {
		t.Fatalf("入群后新提及应为 1/1: %+v", snap.channels[groupID])
	}
}
