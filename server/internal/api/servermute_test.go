package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #36 服务端强制闭麦 REST：权限门、频道形态、自我操作、目标不在房与审计。

// fakeHub 记录 SetServerMuted 调用；按 "channelID|userID" 模拟目标不在房。
// KickChannel 也记录（#37 删频道/分类的踢房断言）；会话变更通知同样记录（#38 B3）。
type fakeHub struct {
	mu    sync.Mutex
	calls []string
	notIn map[string]bool
	kicks []string

	convEvents []convEvent
	msgEvents  []msgEvent
	unreads    []unreadEvent
	audios     []audioEvent
}

// audioEvent 一次 channel.audio 广播的断言快照（#46）。
type audioEvent struct {
	uids      []string
	channelID string
	bitrate   int
	frameMS   int
}

// unreadEvent 一次 channel.unread 广播的断言快照（#40）。
type unreadEvent struct {
	channelID string
	counts    map[string]store.Unread
}

// msgEvent 一次 message.edited / message.deleted 广播的断言快照。
type msgEvent struct {
	kind      string // edited / deleted
	uids      []string
	channelID string
	messageID string
	content   string
	editedAt  int64
	mentions  []string
}

// convEvent 一次 conversation.changed 广播的断言快照。
type convEvent struct {
	uids   []string
	id     string
	action string
}

func (f *fakeHub) BroadcastConversationChanged(uids []string, conversationID, action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.convEvents = append(f.convEvents, convEvent{
		uids:   append([]string(nil), uids...),
		id:     conversationID,
		action: action,
	})
}

// lastConvEvent 最近一次会话变更通知（无则 ok=false）。
func (f *fakeHub) lastConvEvent() (convEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.convEvents) == 0 {
		return convEvent{}, false
	}
	return f.convEvents[len(f.convEvents)-1], true
}

// convLog 全部会话变更通知的快照。
func (f *fakeHub) convLog() []convEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]convEvent(nil), f.convEvents...)
}

func (f *fakeHub) BroadcastMessageEdited(uids []string, channelID, messageID, content string, editedAt int64, mentions []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgEvents = append(f.msgEvents, msgEvent{
		kind:      "edited",
		uids:      append([]string(nil), uids...),
		channelID: channelID,
		messageID: messageID,
		content:   content,
		editedAt:  editedAt,
		mentions:  append([]string(nil), mentions...),
	})
}

func (f *fakeHub) BroadcastMessageDeleted(uids []string, channelID, messageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgEvents = append(f.msgEvents, msgEvent{
		kind:      "deleted",
		uids:      append([]string(nil), uids...),
		channelID: channelID,
		messageID: messageID,
	})
}

// msgLog 全部消息编辑/删除广播的快照。
func (f *fakeHub) msgLog() []msgEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]msgEvent(nil), f.msgEvents...)
}

func (f *fakeHub) BroadcastChannelUnread(channelID string, counts map[string]store.Unread) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]store.Unread, len(counts))
	for uid, u := range counts {
		cp[uid] = u
	}
	f.unreads = append(f.unreads, unreadEvent{channelID: channelID, counts: cp})
}

// unreadLog 全部 channel.unread 广播的快照。
func (f *fakeHub) unreadLog() []unreadEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]unreadEvent, 0, len(f.unreads))
	for _, ev := range f.unreads {
		cp := make(map[string]store.Unread, len(ev.counts))
		for uid, u := range ev.counts {
			cp[uid] = u
		}
		out = append(out, unreadEvent{channelID: ev.channelID, counts: cp})
	}
	return out
}

func (f *fakeHub) BroadcastChannelAudio(uids []string, channelID string, bitrateKbps, frameMS int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audios = append(f.audios, audioEvent{
		uids:      append([]string(nil), uids...),
		channelID: channelID,
		bitrate:   bitrateKbps,
		frameMS:   frameMS,
	})
}

// audioLog 全部 channel.audio 广播的快照。
func (f *fakeHub) audioLog() []audioEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audioEvent(nil), f.audios...)
}

func (f *fakeHub) Kick(uid, dev, reason string) {}

func (f *fakeHub) KickChannel(channelID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks = append(f.kicks, channelID)
}

// kicked 返回被踢频道 id 列表快照。
func (f *fakeHub) kicked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kicks...)
}

func (f *fakeHub) SetServerMuted(channelID, userID string, muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%v", channelID, userID, muted))
	if f.notIn[channelID+"|"+userID] {
		return signal.ErrNotInChannel
	}
	return nil
}

func (f *fakeHub) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

// setupServerMute 一套闭麦测试夹具：owner + member + 语音频道 + fake hub。
func setupServerMute(t *testing.T) (w *permWorld, h *fakeHub, voiceID, memberID int64) {
	t.Helper()
	w = newPermWorld(t)
	h = &fakeHub{notIn: map[string]bool{}}
	w.srv.hub = h
	ch := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "语音房", "type": "voice"}, http.StatusCreated)
	return w, h, asID(ch["id"]), meID(t, w.h, w.member)
}

// TestServerMuteAPI 权限与错误映射 + 成功路径审计。
func TestServerMuteAPI(t *testing.T) {
	w, h, voiceID, memberID := setupServerMute(t)
	rt := w.h
	body := map[string]bool{"muted": true}
	path := fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", voiceID, memberID)

	// 成员无 MuteMembers → 403；非成员 → 403。
	doJSON(t, rt, http.MethodPut, path, w.member, body, http.StatusForbidden)
	doJSON(t, rt, http.MethodPut, path, w.stranger, body, http.StatusForbidden)

	// 频道不存在 404。
	doJSON(t, rt, http.MethodPut, fmt.Sprintf("/api/v1/channels/4242424242/members/%d/server-mute", memberID),
		w.owner, body, http.StatusNotFound)

	// 非语音频道 400（先有权限，owner 过权限门）。
	text := postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "文字", "type": "text"}, http.StatusCreated)
	doJSON(t, rt, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", asID(text["id"]), memberID),
		w.owner, body, http.StatusBadRequest)

	// 自我操作 400。
	ownerID := meID(t, rt, w.owner)
	doJSON(t, rt, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", voiceID, ownerID),
		w.owner, body, http.StatusBadRequest)

	// 目标不在语音房 404。
	h.notIn[strconv.FormatInt(voiceID, 10)+"|"+strconv.FormatInt(memberID, 10)] = true
	doJSON(t, rt, http.MethodPut, path, w.owner, body, http.StatusNotFound)

	// 成功：hub 收到 muted=true，审计 voice.server_mute。
	delete(h.notIn, strconv.FormatInt(voiceID, 10)+"|"+strconv.FormatInt(memberID, 10))
	doJSON(t, rt, http.MethodPut, path, w.owner, body, http.StatusOK)
	if got := h.last(); got != fmt.Sprintf("%d|%d|true", voiceID, memberID) {
		t.Fatalf("hub 调用 = %q", got)
	}
	// 解禁。
	doJSON(t, rt, http.MethodPut, path, w.owner, map[string]bool{"muted": false}, http.StatusOK)
	if got := h.last(); !strings.HasSuffix(got, "|false") {
		t.Fatalf("解禁应 muted=false: %q", got)
	}

	// 审计：voice.server_mute / voice.server_unmute 都有，detail 无隐私字段。
	logs := getJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.owner, http.StatusOK).([]any)
	seen := map[string]bool{}
	for _, raw := range logs {
		e := raw.(map[string]any)
		action, _ := e["action"].(string)
		if action == "voice.server_mute" || action == "voice.server_unmute" {
			seen[action] = true
			detail, _ := e["detail"].(string)
			if strings.Contains(detail, "email") || strings.Contains(detail, "token") {
				t.Fatalf("审计 detail 不得含隐私字段: %s", detail)
			}
		}
	}
	if !seen["voice.server_mute"] || !seen["voice.server_unmute"] {
		t.Fatalf("审计应含 mute/unmute 两类，实际 %v", seen)
	}

	// 非法 body 400（muted 类型错配，解码失败）。
	doJSON(t, rt, http.MethodPut, path, w.owner, map[string]any{"muted": "yes"}, http.StatusBadRequest)
}

// TestServerMuteHubNil 无信令面时按「目标不在房」404（fail-closed，不 500）。
func TestServerMuteHubNil(t *testing.T) {
	w := newPermWorld(t)
	ch := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "语音", "type": "voice"}, http.StatusCreated)
	memberID := meID(t, w.h, w.member)
	doJSON(t, w.h, http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", asID(ch["id"]), memberID),
		w.owner, map[string]bool{"muted": true}, http.StatusNotFound)
}

// TestServerMuteHierarchy 层级闸（#36 验收追加）：非 owner 不能对同级及以上成员
// 强制闭麦——admin 闭麦 owner 后对方无法自助解禁（self 被拒），必须拦在前面。
func TestServerMuteHierarchy(t *testing.T) {
	w, h, voiceID, memberID := setupServerMute(t)
	rt := w.h
	ctx := context.Background()
	// 第二个 admin（直写 store 挂 builtin）。
	admin2 := mustReg(t, rt, w.mem, "sm-a2@ex.com", "smAdmin2")
	admin2ID := meID(t, rt, admin2)
	if err := w.mem.AddMember(ctx, &store.ServerMember{ServerID: w.serverID, UserID: admin2ID, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("AddMember(admin2): %v", err)
	}
	ownerID := meID(t, rt, w.owner)
	adminID := meID(t, rt, w.admin)
	body := map[string]bool{"muted": true}
	at := func(target int64) string {
		return fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", voiceID, target)
	}

	// admin 闭麦 owner / 另一个 admin → 403。
	doJSON(t, rt, http.MethodPut, at(ownerID), w.admin, body, http.StatusForbidden)
	doJSON(t, rt, http.MethodPut, at(admin2ID), w.admin, body, http.StatusForbidden)
	// self 保持 400（先于层级：对自己操作是更具体的错误）。
	doJSON(t, rt, http.MethodPut, at(adminID), w.admin, body, http.StatusBadRequest)

	// admin 闭麦普通成员 → 200。
	doJSON(t, rt, http.MethodPut, at(memberID), w.admin, body, http.StatusOK)
	if got := h.last(); got != fmt.Sprintf("%d|%d|true", voiceID, memberID) {
		t.Fatalf("hub 调用 = %q", got)
	}
	// owner 闭麦 admin → 200（owner 不受层级限制）。
	doJSON(t, rt, http.MethodPut, at(admin2ID), w.owner, body, http.StatusOK)

	// 目标不是成员 → 404（admin 与 owner 同口径）。
	doJSON(t, rt, http.MethodPut, at(4242424242), w.admin, body, http.StatusNotFound)
	doJSON(t, rt, http.MethodPut, at(4242424242), w.owner, body, http.StatusNotFound)
}
