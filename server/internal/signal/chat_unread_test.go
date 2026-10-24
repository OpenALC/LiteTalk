package signal

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #40 B2：chat.send 落库后给每个收件人推自己的 channel.unread。

// recordingChatStore 包一层 Mem，记录 UnreadCountsForUsers 调用；result 非 nil
// 时返回可控值（单元测试用），否则走真实 Mem 计数（端到端断言用）。
type recordingChatStore struct {
	*store.Mem
	mu     sync.Mutex
	calls  []unreadCall
	result map[int64]store.Unread
	err    error
}

type unreadCall struct {
	channelID int64
	uids      []int64
}

func (r *recordingChatStore) UnreadCountsForUsers(ctx context.Context, channelID int64, userIDs []int64) (map[int64]store.Unread, error) {
	ids := append([]int64(nil), userIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	r.mu.Lock()
	r.calls = append(r.calls, unreadCall{channelID: channelID, uids: ids})
	result, err := r.result, r.err
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if result != nil {
		out := make(map[int64]store.Unread, len(result))
		for uid, u := range result {
			out[uid] = u
		}
		return out, nil
	}
	return r.Mem.UnreadCountsForUsers(ctx, channelID, ids)
}

func (r *recordingChatStore) unreadCalls() []unreadCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]unreadCall(nil), r.calls...)
}

// seqOfChat 第 n 条 chat.message 在单用户会话事件环里的 seq。每条 chat.message
// 后紧跟该用户自己的 channel.unread（#40），因此 chat 事件的 seq 步长为 2。
func seqOfChat(n int64) int64 { return 2*n - 1 }

// TestChatSendBroadcastsChannelUnread 提及消息：双方先收 chat.message，再收各自的
// channel.unread（alice 1/1，发送者 bob 0/0），且计数按收件人集合一次批量计算。
func TestChatSendBroadcastsChannelUnread(t *testing.T) {
	f := newChatFixture(t)
	rec := &recordingChatStore{Mem: f.st}
	f.hub.chatStore = rec

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	chID := strconv.FormatInt(f.ch.ID, 10)

	b.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "@alice 看这里", Mentions: []string{"1"},
	})

	// 消息先于计数事件到达（服务端先 broadcastEvent(chat.message)，再推计数）。
	for _, c := range []*wsClient{a, b} {
		if env := c.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("%s 未收到 chat.message", c.user)
		}
	}
	var gotA, gotB ChannelUnreadData
	if err := a.recvUntil(TypeChannelUnread).Decode(&gotA); err != nil {
		t.Fatalf("alice decode channel.unread: %v", err)
	}
	if err := b.recvUntil(TypeChannelUnread).Decode(&gotB); err != nil {
		t.Fatalf("bob decode channel.unread: %v", err)
	}
	if gotA.ChannelID != chID || gotA.UnreadCount != 1 || gotA.MentionCount != 1 {
		t.Fatalf("alice channel.unread = %+v，期望 1/1", gotA)
	}
	if gotB.ChannelID != chID || gotB.UnreadCount != 0 || gotB.MentionCount != 0 {
		t.Fatalf("bob channel.unread = %+v，期望 0/0", gotB)
	}

	// 一次调用覆盖 chat.message 的全部收件人（含发送者；mallory 也是服务器成员）。
	calls := rec.unreadCalls()
	if len(calls) != 1 {
		t.Fatalf("应调用 1 次 UnreadCountsForUsers，实际 %d", len(calls))
	}
	if calls[0].channelID != f.ch.ID {
		t.Fatalf("channelID = %d，期望 %d", calls[0].channelID, f.ch.ID)
	}
	if len(calls[0].uids) < 2 || calls[0].uids[0] != 1 || calls[0].uids[1] != 2 {
		t.Fatalf("收件人集合应含 alice/bob: %v", calls[0].uids)
	}
}

// TestBroadcastChannelUnreadPerRecipient 直接广播时每个 uid 只收自己的计数；
// 空 map 不产生事件。
func TestBroadcastChannelUnreadPerRecipient(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	m := f.dial("9", "mallory")

	f.hub.BroadcastChannelUnread("77", map[string]store.Unread{
		"1": {UnreadCount: 2, MentionCount: 1},
		"2": {UnreadCount: 3, MentionCount: 0},
	})
	var ua, ub ChannelUnreadData
	if err := a.recvUntil(TypeChannelUnread).Decode(&ua); err != nil {
		t.Fatalf("alice decode: %v", err)
	}
	if err := b.recvUntil(TypeChannelUnread).Decode(&ub); err != nil {
		t.Fatalf("bob decode: %v", err)
	}
	if ua.UnreadCount != 2 || ua.MentionCount != 1 || ua.ChannelID != "77" {
		t.Fatalf("alice 计数不符: %+v", ua)
	}
	if ub.UnreadCount != 3 || ub.MentionCount != 0 || ub.ChannelID != "77" {
		t.Fatalf("bob 计数不符: %+v", ub)
	}

	// mallory 不在 counts 里：下一条事件必须是另一个类型（若泄漏会先读到 unread）。
	f.hub.BroadcastChannelUnread("77", nil)
	f.hub.BroadcastMessageDeleted([]string{"9"}, "77", "88")
	if env := m.recvUntil(TypeMessageDeleted); env.Type != TypeMessageDeleted {
		t.Fatalf("mallory 不应收到 channel.unread，实际先收到 %q", env.Type)
	}
}
