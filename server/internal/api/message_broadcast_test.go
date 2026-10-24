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

// #39 B3：REST 编辑/软删成功后的 message.edited / message.deleted 广播。
// 目标口径：服务器频道 = 有 ViewChannel 的成员；会话频道 = 全部会话成员。

// assertUIDSet 断言广播 uid 集合（顺序不敏感）。
func assertUIDSet(t *testing.T, got []string, want ...int64) {
	t.Helper()
	wantIDs := make([]string, 0, len(want))
	for _, id := range want {
		wantIDs = append(wantIDs, strconv.FormatInt(id, 10))
	}
	sort.Strings(got)
	sort.Strings(wantIDs)
	if len(got) != len(wantIDs) {
		t.Fatalf("广播 uids = %v，期望 %v", got, wantIDs)
	}
	for i := range wantIDs {
		if got[i] != wantIDs[i] {
			t.Fatalf("广播 uids = %v，期望 %v", got, wantIDs)
		}
	}
}

// assertMentionStrings 断言广播 mentions（服务端按 user_id 升序）。
func assertMentionStrings(t *testing.T, got []string, want ...int64) {
	t.Helper()
	wantIDs := make([]string, 0, len(want))
	for _, id := range want {
		wantIDs = append(wantIDs, strconv.FormatInt(id, 10))
	}
	sort.Strings(wantIDs)
	if len(got) != len(wantIDs) {
		t.Fatalf("广播 mentions = %v，期望 %v", got, wantIDs)
	}
	for i := range wantIDs {
		if got[i] != wantIDs[i] {
			t.Fatalf("广播 mentions = %v，期望 %v", got, wantIDs)
		}
	}
}

// TestMessageEditDeleteBroadcastServerChannel 服务器频道：编辑/删除广播带正确
// payload 与可见成员；无 ViewChannel 的成员被过滤；失败路径不广播。
func TestMessageEditDeleteBroadcastServerChannel(t *testing.T) {
	w, chID, ownerID, adminID, memberID := newReplyWorld(t)
	hub := &fakeHub{}
	w.srv.hub = hub

	msgID := sendMsg(t, w.h, w.member, chID, "原始内容")
	path := fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, msgID)

	// 编辑：全部成员（owner/admin/member）都有 ViewChannel。
	patchJSON(t, w.h, path, w.member,
		map[string]any{"content": "编辑后", "mentions": uidList(ownerID, memberID)}, http.StatusOK)
	events := hub.msgLog()
	if len(events) != 1 {
		t.Fatalf("编辑应产生 1 次广播，实际 %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.kind != "edited" || ev.channelID != strconv.FormatInt(chID, 10) ||
		ev.messageID != strconv.FormatInt(msgID, 10) || ev.content != "编辑后" || ev.editedAt <= 0 {
		t.Fatalf("message.edited payload 不符: %+v", ev)
	}
	assertUIDSet(t, ev.uids, ownerID, adminID, memberID)
	assertMentionStrings(t, ev.mentions, ownerID, memberID)

	// 失败路径不广播：非作者编辑 403。
	deniedPath := fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID,
		sendMsg(t, w.h, w.member, chID, "别人的消息"))
	patchJSON(t, w.h, deniedPath, w.admin, map[string]any{"content": "越权"}, http.StatusForbidden)
	if got := hub.msgLog(); len(got) != 1 {
		t.Fatalf("失败编辑不应广播，实际 %d 次: %+v", len(got), got)
	}

	// 对 member 拒绝 ViewChannel：后续广播不再包含 member。
	ctx := context.Background()
	if err := w.mem.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: chID, TargetType: store.OverwriteTargetMember,
		TargetID: memberID, Deny: store.ViewChannel,
	}); err != nil {
		t.Fatalf("SetChannelOverwrite: %v", err)
	}
	w.srv.authz.InvalidateServer(w.serverID)

	ownerMsg := sendMsg(t, w.h, w.owner, chID, "服主消息")
	ownerPath := fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, ownerMsg)
	patchJSON(t, w.h, ownerPath, w.owner, map[string]any{"content": "服主改"}, http.StatusOK)
	events = hub.msgLog()
	if len(events) != 2 {
		t.Fatalf("第二次编辑应追加广播，实际 %d: %+v", len(events), events)
	}
	assertUIDSet(t, events[1].uids, ownerID, adminID)
	if len(events[1].mentions) != 0 {
		t.Fatalf("无提及时广播 mentions 应为空: %v", events[1].mentions)
	}

	// 删除：同一可见性口径。
	delJSON(t, w.h, ownerPath, w.owner, http.StatusNoContent)
	events = hub.msgLog()
	if len(events) != 3 {
		t.Fatalf("删除应追加广播，实际 %d: %+v", len(events), events)
	}
	del := events[2]
	if del.kind != "deleted" || del.channelID != strconv.FormatInt(chID, 10) ||
		del.messageID != strconv.FormatInt(ownerMsg, 10) {
		t.Fatalf("message.deleted payload 不符: %+v", del)
	}
	assertUIDSet(t, del.uids, ownerID, adminID)

	// 重复删除 404，不再广播。
	delJSON(t, w.h, ownerPath, w.owner, http.StatusNotFound)
	if got := hub.msgLog(); len(got) != 3 {
		t.Fatalf("失败删除不应广播，实际 %d 次: %+v", len(got), got)
	}
}

// TestMessageEditDeleteBroadcastConversation 会话频道：广播目标是全部会话成员，
// 非成员不收；编辑保留/替换提及同样回填。
func TestMessageEditDeleteBroadcastConversation(t *testing.T) {
	w := newConvWorld(t)
	hub := &fakeHub{}
	w.srv.hub = hub

	convID := asID(postJSON(t, w.h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.bobID)}, http.StatusCreated)["id"])
	msgID := sendMsg(t, w.h, w.alice, convID, "私聊原消息")
	path := fmt.Sprintf("/api/v1/channels/%d/messages/%d", convID, msgID)

	patchJSON(t, w.h, path, w.alice,
		map[string]any{"content": "私聊改", "mentions": uidList(w.bobID)}, http.StatusOK)
	events := hub.msgLog()
	if len(events) != 1 {
		t.Fatalf("编辑应产生 1 次广播，实际 %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.kind != "edited" || ev.channelID != strconv.FormatInt(convID, 10) ||
		ev.messageID != strconv.FormatInt(msgID, 10) || ev.content != "私聊改" || ev.editedAt <= 0 {
		t.Fatalf("message.edited payload 不符: %+v", ev)
	}
	assertUIDSet(t, ev.uids, w.aliceID, w.bobID)
	assertMentionStrings(t, ev.mentions, w.bobID)

	delJSON(t, w.h, path, w.alice, http.StatusNoContent)
	events = hub.msgLog()
	if len(events) != 2 || events[1].kind != "deleted" {
		t.Fatalf("删除应追加广播，实际 %+v", events)
	}
	assertUIDSet(t, events[1].uids, w.aliceID, w.bobID)
	if events[1].channelID != strconv.FormatInt(convID, 10) ||
		events[1].messageID != strconv.FormatInt(msgID, 10) {
		t.Fatalf("message.deleted payload 不符: %+v", events[1])
	}
}
