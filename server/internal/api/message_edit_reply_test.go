package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #39 B2：消息编辑/软删/回复/@提及 REST 与提及候选。
// 权限语义：编辑仅作者；删除 = 作者，或服务器频道 ManageServer（管理员/服主），
// 会话频道仅作者；提及候选按频道可见性过滤。

// mentionIDs 取消息响应里的 mentions（ID 字符串数组）；字段缺省返回 nil。
func mentionIDs(t *testing.T, msg map[string]any) []int64 {
	t.Helper()
	raw, ok := msg["mentions"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("mentions 不是数组: %v", raw)
	}
	out := make([]int64, 0, len(arr))
	for _, v := range arr {
		out = append(out, asID(v))
	}
	return out
}

// replyPreviewOf 取 reply_preview；字段缺省返回 nil。
func replyPreviewOf(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	raw, ok := msg["reply_preview"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("reply_preview 不是对象: %v", raw)
	}
	return m
}

// findMessage 在列表响应中按 ID 找一条消息。
func findMessage(t *testing.T, list []any, id int64) map[string]any {
	t.Helper()
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("消息不是对象: %v", raw)
		}
		if asID(m["id"]) == id {
			return m
		}
	}
	t.Fatalf("列表里找不到消息 %d: %v", id, list)
	return nil
}

// assertMentions 断言 mentions 与期望集合一致（服务端按 user_id 升序返回）。
func assertMentions(t *testing.T, msg map[string]any, want ...int64) {
	t.Helper()
	got := mentionIDs(t, msg)
	sorted := append([]int64(nil), want...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if len(got) != len(sorted) {
		t.Fatalf("mentions = %v，期望 %v", got, sorted)
	}
	for i := range sorted {
		if got[i] != sorted[i] {
			t.Fatalf("mentions = %v，期望 %v", got, sorted)
		}
	}
}

// msgWorld 消息夹具：用户直写 Store（跳过注册的 bcrypt——CI 的 -race 下注册
// 单次要约 3 秒，四个用户就会吃掉整个包的超时余量），服务器经真实 API 创建，
// admin/member 直写成员后失效权限缓存（与权限矩阵夹具同口径）。
type msgWorld struct {
	srv        *Server
	h          http.Handler
	mem        *store.Mem
	serverID   int64
	owner      string
	admin      string
	member     string
	stranger   string
	ownerID    int64
	adminID    int64
	memberID   int64
	strangerID int64
}

func newMsgWorld(t *testing.T) *msgWorld {
	t.Helper()
	srv, mem := newTestServer(t)
	h := srv.Handler()
	w := &msgWorld{srv: srv, h: h, mem: mem}
	w.owner, w.ownerID = addUser(t, srv, mem, "mw-o@ex.com", "mwOwner")
	w.admin, w.adminID = addUser(t, srv, mem, "mw-a@ex.com", "mwAdmin")
	w.member, w.memberID = addUser(t, srv, mem, "mw-m@ex.com", "mwMember")
	w.stranger, w.strangerID = addUser(t, srv, mem, "mw-x@ex.com", "mwStranger")
	created := postJSON(t, h, "/api/v1/servers", w.owner,
		map[string]string{"name": "消息测试服", "icon_name": "music", "icon_color": "#4FA4F3"},
		http.StatusCreated)
	w.serverID = asID(created["id"])
	ctx := context.Background()
	if err := mem.AddMember(ctx, &store.ServerMember{
		ServerID: w.serverID, UserID: w.adminID, Role: store.RoleAdmin,
	}); err != nil {
		t.Fatalf("AddMember(admin): %v", err)
	}
	if err := mem.AddMember(ctx, &store.ServerMember{
		ServerID: w.serverID, UserID: w.memberID, Role: store.RoleMember,
	}); err != nil {
		t.Fatalf("AddMember(member): %v", err)
	}
	srv.authz.InvalidateServer(w.serverID)
	return w
}

// newReplyWorld 建服 + 文本频道，返回频道 ID 与 owner/admin/member 用户 ID。
func newReplyWorld(t *testing.T) (*msgWorld, int64, int64, int64, int64) {
	t.Helper()
	w := newMsgWorld(t)
	chID := asID(postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "回复大厅", "type": "text"}, http.StatusCreated)["id"])
	return w, chID, w.ownerID, w.adminID, w.memberID
}

// TestMessageReplyPreviewAndMentions 创建带 reply/mentions 的消息、列表/单条回读、
// 跨频道与不存在的回复目标、非法提及、软删目标预览、nonce 幂等回填。
func TestMessageReplyPreviewAndMentions(t *testing.T) {
	w, chID, ownerID, _, memberID := newReplyWorld(t)
	h := w.h
	strangerID := w.strangerID

	orig := sendMsg(t, h, w.owner, chID, "原消息")

	// 回复 + 提及：重复项去重，响应带 reply_preview 与 mentions。
	resp := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
		map[string]any{
			"content":  "回复你",
			"reply_to": strconv.FormatInt(orig, 10),
			"mentions": uidList(ownerID, memberID, ownerID),
		}, http.StatusCreated)
	replyID := asID(resp["id"])
	if asID(resp["reply_to"]) != orig {
		t.Fatalf("reply_to = %v，期望 %d", resp["reply_to"], orig)
	}
	prev := replyPreviewOf(t, resp)
	if prev == nil {
		t.Fatal("回复消息缺 reply_preview")
	}
	if asID(prev["id"]) != orig || asID(prev["user_id"]) != ownerID ||
		prev["nickname"] != "mwOwner" || prev["content"] != "原消息" || prev["deleted"] != false {
		t.Fatalf("reply_preview 不符: %v", prev)
	}
	assertMentions(t, resp, ownerID, memberID)

	// 列表与单条都带新字段，且与创建响应一致。
	listed := findMessage(t, getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID),
		w.owner, http.StatusOK).([]any), replyID)
	if asID(listed["reply_to"]) != orig {
		t.Fatalf("列表 reply_to = %v", listed["reply_to"])
	}
	if p := replyPreviewOf(t, listed); p == nil || asID(p["id"]) != orig || p["deleted"] != false {
		t.Fatalf("列表 reply_preview 不符: %v", p)
	}
	assertMentions(t, listed, ownerID, memberID)

	single := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, replyID),
		w.admin, http.StatusOK).(map[string]any)
	if p := replyPreviewOf(t, single); p == nil || p["content"] != "原消息" {
		t.Fatalf("单条 reply_preview 不符: %v", p)
	}
	assertMentions(t, single, ownerID, memberID)

	// 跨频道回复 → 400 bad_reply；不存在 / 0 同样。
	ch2 := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "别的频道", "type": "text"}, http.StatusCreated)["id"])
	other := sendMsg(t, h, w.owner, ch2, "别的频道的消息")
	for _, rt := range []string{strconv.FormatInt(other, 10), "999999999", "0"} {
		body := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
			map[string]any{"content": "坏回复", "reply_to": rt}, http.StatusBadRequest)
		if body["code"] != "bad_reply" {
			t.Fatalf("reply_to=%s code = %v，期望 bad_reply", rt, body["code"])
		}
	}

	// 提及非成员 / 0 → 400 bad_mention。
	for _, ids := range [][]string{uidList(strangerID), {"0"}} {
		body := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
			map[string]any{"content": "坏提及", "mentions": ids}, http.StatusBadRequest)
		if body["code"] != "bad_mention" {
			t.Fatalf("mentions=%v code = %v，期望 bad_mention", ids, body["code"])
		}
	}

	// 被引用消息已软删也允许回复：预览 content 为空、deleted=true。
	gone := sendMsg(t, h, w.owner, chID, "将被删除")
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, gone), w.owner, http.StatusNoContent)
	late := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
		map[string]any{"content": "迟到的回复", "reply_to": strconv.FormatInt(gone, 10)}, http.StatusCreated)
	if p := replyPreviewOf(t, late); p == nil || p["deleted"] != true || p["content"] != "" {
		t.Fatalf("软删目标预览不符: %v", p)
	}

	// 单条：跨频道消息 ID 在路径频道下 404；服务器频道非成员 403。
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, other), w.owner, http.StatusNotFound)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, replyID), w.stranger, http.StatusForbidden)

	// nonce 幂等重放：原消息的 mentions/reply_preview 原样回填（不用重放请求体）。
	first := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
		map[string]any{
			"content":  "幂等回复",
			"nonce":    "rp-1",
			"reply_to": strconv.FormatInt(orig, 10),
			"mentions": uidList(ownerID),
		}, http.StatusCreated)
	replay := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
		map[string]any{"content": "试图替换", "nonce": "rp-1", "mentions": []string{}},
		http.StatusOK)
	if asID(replay["id"]) != asID(first["id"]) || replay["content"] != "幂等回复" {
		t.Fatalf("幂等重放应回原消息: %v", replay)
	}
	if p := replyPreviewOf(t, replay); p == nil || asID(p["id"]) != orig {
		t.Fatalf("幂等重放应回填 reply_preview: %v", replyPreviewOf(t, replay))
	}
	assertMentions(t, replay, ownerID)
}

// TestMessageEdit 编辑权限/长度/提及三态（缺省不变、给出替换、空数组清除）/
// 已删除消息/跨频道消息。
func TestMessageEdit(t *testing.T) {
	w, chID, ownerID, adminID, _ := newReplyWorld(t)
	h := w.h
	strangerID := w.strangerID

	msg := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member,
		map[string]any{"content": "原始内容", "mentions": uidList(ownerID)}, http.StatusCreated)
	msgID := asID(msg["id"])
	path := fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, msgID)

	// 非作者 403。
	deny := patchJSON(t, h, path, w.owner, map[string]any{"content": "越权编辑"}, http.StatusForbidden)
	if deny["code"] != "forbidden" {
		t.Fatalf("非作者 code = %v", deny["code"])
	}

	// mentions 缺省 = 保持原提及；edited_at 置位。
	edited := patchJSON(t, h, path, w.member, map[string]any{"content": "改过一次自"},
		http.StatusOK)
	if edited["content"] != "改过一次自" {
		t.Fatalf("编辑后 content = %v", edited["content"])
	}
	if ts, ok := edited["edited_at"].(string); !ok || ts == "" {
		t.Fatalf("编辑后缺 edited_at: %v", edited)
	}
	assertMentions(t, edited, ownerID)

	// 给出 mentions = 整体替换。
	replaced := patchJSON(t, h, path, w.member,
		map[string]any{"content": "换提及", "mentions": uidList(adminID)}, http.StatusOK)
	assertMentions(t, replaced, adminID)

	// 空数组 = 清除全部提及（字段缺省）。
	cleared := patchJSON(t, h, path, w.member,
		map[string]any{"content": "清空提及", "mentions": []string{}}, http.StatusOK)
	if ids := mentionIDs(t, cleared); len(ids) != 0 {
		t.Fatalf("空数组应清除提及，实际 %v", ids)
	}

	// 非法提及 → bad_mention；内容长度越界 → bad_content。
	bad := patchJSON(t, h, path, w.member,
		map[string]any{"content": "坏提及", "mentions": uidList(strangerID)}, http.StatusBadRequest)
	if bad["code"] != "bad_mention" {
		t.Fatalf("非法提及 code = %v", bad["code"])
	}
	for _, content := range []string{"", strings.Repeat("x", 2001)} {
		body := patchJSON(t, h, path, w.member, map[string]any{"content": content},
			http.StatusBadRequest)
		if body["code"] != "bad_content" {
			t.Fatalf("content 长度 %d code = %v", len([]rune(content)), body["code"])
		}
	}

	// 跨频道消息 ID → 404（路径频道下）。
	ch2 := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "另一频道", "type": "text"}, http.StatusCreated)["id"])
	other := sendMsg(t, h, w.member, ch2, "另一条")
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, other), w.member,
		map[string]any{"content": "错频道"}, http.StatusNotFound)
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, 999999999), w.member,
		map[string]any{"content": "不存在"}, http.StatusNotFound)

	// 已删除消息不可编辑 → 404 message_deleted。
	delJSON(t, h, path, w.member, http.StatusNoContent)
	deleted := patchJSON(t, h, path, w.member, map[string]any{"content": "死后再编辑"},
		http.StatusNotFound)
	if deleted["code"] != "message_deleted" {
		t.Fatalf("已删除编辑 code = %v", deleted["code"])
	}
}

// TestMessageDeletePermissions 作者/管理员/普通成员/非成员，重复删除与软删视图。
func TestMessageDeletePermissions(t *testing.T) {
	w, chID, _, _, _ := newReplyWorld(t)
	h := w.h

	delPath := func(id int64) string {
		return fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, id)
	}

	// 作者删除自己的消息；重复删除 404 message_deleted。
	own := sendMsg(t, h, w.member, chID, "自己的")
	delJSON(t, h, delPath(own), w.member, http.StatusNoContent)
	again := delJSON(t, h, delPath(own), w.member, http.StatusNotFound)
	if again["code"] != "message_deleted" {
		t.Fatalf("重复删除 code = %v", again["code"])
	}

	// 软删视图：列表与单条 content 为空、deleted_at 有值。
	listed := findMessage(t, getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID),
		w.member, http.StatusOK).([]any), own)
	if listed["content"] != "" || listed["deleted_at"] == nil {
		t.Fatalf("软删消息视图不符: %v", listed)
	}
	single := getJSON(t, h, delPath(own), w.member, http.StatusOK).(map[string]any)
	if single["content"] != "" || single["deleted_at"] == nil {
		t.Fatalf("软删单条视图不符: %v", single)
	}

	// 管理员 / 服主可删他人消息。
	byMember := sendMsg(t, h, w.member, chID, "管理员删")
	delJSON(t, h, delPath(byMember), w.admin, http.StatusNoContent)
	byMember2 := sendMsg(t, h, w.member, chID, "服主删")
	delJSON(t, h, delPath(byMember2), w.owner, http.StatusNoContent)

	// 普通成员不能删他人（管理员发的）；非成员不能删（403，无 ViewChannel）。
	byAdmin := sendMsg(t, h, w.admin, chID, "普通成员删不掉")
	deny := delJSON(t, h, delPath(byAdmin), w.member, http.StatusForbidden)
	if deny["code"] != "forbidden" {
		t.Fatalf("普通成员删他人 code = %v", deny["code"])
	}
	delJSON(t, h, delPath(byAdmin), w.stranger, http.StatusForbidden)
}

// TestMessageEditDeleteRateLimit 编辑与删除各 20 次/分/用户。
func TestMessageEditDeleteRateLimit(t *testing.T) {
	w, chID, _, _, _ := newReplyWorld(t)
	h := w.h

	// 编辑频控：同一消息连续 20 次成功，第 21 次 429。
	msgID := sendMsg(t, h, w.owner, chID, "频控")
	path := fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, msgID)
	for i := 0; i < 20; i++ {
		patchJSON(t, h, path, w.owner,
			map[string]any{"content": fmt.Sprintf("第%d次", i)}, http.StatusOK)
	}
	over := patchJSON(t, h, path, w.owner, map[string]any{"content": "超限"},
		http.StatusTooManyRequests)
	if over["code"] != "rate_limited" {
		t.Fatalf("编辑超限 code = %v", over["code"])
	}

	// 删除频控用独立用户：admin 发 20 条、owner 发 1 条，admin 连删 21 条。
	ids := make([]int64, 0, 21)
	for i := 0; i < 20; i++ {
		ids = append(ids, sendMsg(t, h, w.admin, chID, fmt.Sprintf("待删%d", i)))
	}
	ids = append(ids, sendMsg(t, h, w.owner, chID, "owner 发的"))
	for _, id := range ids[:20] {
		delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, id),
			w.admin, http.StatusNoContent)
	}
	overDel := delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, ids[20]),
		w.admin, http.StatusTooManyRequests)
	if overDel["code"] != "rate_limited" {
		t.Fatalf("删除超限 code = %v", overDel["code"])
	}
}

// TestMentionCandidatesServerChannel 服务器频道：有 ViewChannel 的成员，按昵称排序，
// 无 ViewChannel 的成员被过滤，非成员 403，载荷不含 email。
func TestMentionCandidatesServerChannel(t *testing.T) {
	w, chID, ownerID, adminID, memberID := newReplyWorld(t)
	h := w.h

	list := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/mention-candidates", chID),
		w.member, http.StatusOK).([]any)
	if len(list) != 3 {
		t.Fatalf("候选应为 owner/admin/member 3 人，实际 %d: %v", len(list), list)
	}
	wantNames := []string{"mwAdmin", "mwMember", "mwOwner"}
	wantIDs := []int64{adminID, memberID, ownerID}
	for i, raw := range list {
		m := raw.(map[string]any)
		if _, leaked := m["email"]; leaked {
			t.Fatalf("候选泄露 email: %v", m)
		}
		if m["nickname"] != wantNames[i] || asID(m["user_id"]) != wantIDs[i] {
			t.Fatalf("第 %d 个候选 = %v，期望 %s/%d", i, m, wantNames[i], wantIDs[i])
		}
		if tag, ok := m["tag"].(string); !ok || len(tag) != 4 {
			t.Fatalf("候选 tag 应为 4 位字符串: %v", m["tag"])
		}
	}

	// 对 member 个人拒绝 ViewChannel 后不再出现在候选中。
	if err := w.mem.SetChannelOverwrite(context.Background(), &store.ChannelOverwrite{
		ChannelID: chID, TargetType: store.OverwriteTargetMember,
		TargetID: memberID, Deny: store.ViewChannel,
	}); err != nil {
		t.Fatalf("SetChannelOverwrite: %v", err)
	}
	w.srv.authz.InvalidateServer(w.serverID)
	list = getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/mention-candidates", chID),
		w.owner, http.StatusOK).([]any)
	if len(list) != 2 {
		t.Fatalf("过滤后应 2 人，实际 %d: %v", len(list), list)
	}
	for _, raw := range list {
		if asID(raw.(map[string]any)["user_id"]) == memberID {
			t.Fatalf("无 ViewChannel 的成员不应出现: %v", raw)
		}
	}

	// 非成员（无 ViewChannel）403。
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/mention-candidates", chID),
		w.stranger, http.StatusForbidden)
}

// TestConversationMessageEditDeleteReply 会话频道的编辑/删除权限、回复、提及候选
// 与非参与者 404。
func TestConversationMessageEditDeleteReply(t *testing.T) {
	w := newConvWorld(t)
	h := w.h
	created := postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.bobID)}, http.StatusCreated)
	convID := asID(created["id"])
	msgPath := func(id int64) string {
		return fmt.Sprintf("/api/v1/channels/%d/messages/%d", convID, id)
	}

	msgID := sendMsg(t, h, w.alice, convID, "私聊消息")

	// 只有作者能编辑/删除：另一参与者 403。
	patchJSON(t, h, msgPath(msgID), w.bob, map[string]any{"content": "改不了"}, http.StatusForbidden)
	delJSON(t, h, msgPath(msgID), w.bob, http.StatusForbidden)

	// 作者编辑成功。
	edited := patchJSON(t, h, msgPath(msgID), w.alice, map[string]any{"content": "改好了"},
		http.StatusOK)
	if edited["content"] != "改好了" || edited["edited_at"] == nil {
		t.Fatalf("私聊编辑响应不符: %v", edited)
	}

	// 回复 + 提及会话成员。
	reply := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.bob,
		map[string]any{
			"content":  "收到",
			"reply_to": strconv.FormatInt(msgID, 10),
			"mentions": uidList(w.aliceID),
		}, http.StatusCreated)
	if p := replyPreviewOf(t, reply); p == nil || p["nickname"] != "cvAlice" {
		t.Fatalf("私聊回复预览不符: %v", p)
	}
	assertMentions(t, reply, w.aliceID)

	// 提及非会话成员 → bad_mention。
	bad := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.bob,
		map[string]any{"content": "外部人", "mentions": uidList(w.erinID)}, http.StatusBadRequest)
	if bad["code"] != "bad_mention" {
		t.Fatalf("会话外提及 code = %v", bad["code"])
	}

	// 提及候选 = 会话成员，按昵称升序；非参与者 404。
	cands := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/mention-candidates", convID),
		w.bob, http.StatusOK).([]any)
	if len(cands) != 2 {
		t.Fatalf("私聊候选应 2 人，实际 %d", len(cands))
	}
	if c0, c1 := cands[0].(map[string]any), cands[1].(map[string]any); c0["nickname"] != "cvAlice" || c1["nickname"] != "cvBob" {
		t.Fatalf("候选排序不符: %v", cands)
	}
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/mention-candidates", convID),
		w.erin, http.StatusNotFound)
	getJSON(t, h, msgPath(msgID), w.erin, http.StatusNotFound)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.erin, http.StatusNotFound)

	// 作者删除后：重复删除 404；编辑 404 message_deleted；仍可回复（预览显示已删除）。
	delJSON(t, h, msgPath(msgID), w.alice, http.StatusNoContent)
	again := delJSON(t, h, msgPath(msgID), w.alice, http.StatusNotFound)
	if again["code"] != "message_deleted" {
		t.Fatalf("重复删除 code = %v", again["code"])
	}
	de := patchJSON(t, h, msgPath(msgID), w.alice, map[string]any{"content": "x"}, http.StatusNotFound)
	if de["code"] != "message_deleted" {
		t.Fatalf("已删除编辑 code = %v", de["code"])
	}
	late := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), w.bob,
		map[string]any{"content": "还能回复", "reply_to": strconv.FormatInt(msgID, 10)},
		http.StatusCreated)
	if p := replyPreviewOf(t, late); p == nil || p["deleted"] != true || p["content"] != "" {
		t.Fatalf("已删除回复预览不符: %v", p)
	}
}
