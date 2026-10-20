package signal

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #35 B3：WS 鉴权接入。覆盖写入走 store 夹具（B4 才有覆盖管理 API），
// 直写后必须 InvalidateServer——Hub 与 api 共享的 Authorizer 按服缓存。

// stubAuthz 按 uid 注入拒绝/故障（广播过滤的失败路径用真 Authorizer 造不出来）。
type stubAuthz struct {
	mu    sync.Mutex
	deny  map[int64]bool
	fail  map[int64]bool
	calls map[int64]int
}

func (s *stubAuthz) CanInServer(_ context.Context, uid, _, _ int64, _ store.Permission) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[int64]int{}
	}
	s.calls[uid]++
	if s.fail[uid] {
		return false, errors.New("stub: 权限查询故障")
	}
	return !s.deny[uid], nil
}

func (s *stubAuthz) InvalidateServer(int64) {}

// denyOnChannel 对指定成员写个人覆盖并失效缓存。
func denyOnChannel(t *testing.T, f *chatFixture, chID, uid int64, perm store.Permission) {
	t.Helper()
	if err := f.st.SetChannelOverwrite(context.Background(), &store.ChannelOverwrite{
		ChannelID: chID, TargetType: store.OverwriteTargetMember,
		TargetID: uid, Deny: perm,
	}); err != nil {
		t.Fatal(err)
	}
	f.hub.authz.InvalidateServer(f.ch.ServerID)
}

// wantError 断言收到的 error 信封错误码。
func wantError(t *testing.T, c *wsClient, want string) {
	t.Helper()
	env := c.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != want {
		t.Fatalf("code = %q, want %q", e.Code, want)
	}
}

// TestChannelJoinPerm 进房权限门：无 VIEW / 无 CONNECT → forbidden；
// 文本频道 → forbidden；不存在 → not_found（既有错误优先级保持）。
func TestChannelJoinPerm(t *testing.T) {
	t.Run("无 ViewChannel", func(t *testing.T) {
		f := newChatFixture(t)
		denyOnChannel(t, f, f.vch.ID, 2, store.ViewChannel)
		b := f.dial("2", "bob")
		b.send(TypeChannelJoin, ChannelJoinData{ChannelID: strconv.FormatInt(f.vch.ID, 10)})
		wantError(t, b, ErrCodeForbidden)
	})
	t.Run("无 ConnectVoice", func(t *testing.T) {
		f := newChatFixture(t)
		// 只拒 CONNECT：VIEW 通过，卡在第二道门。
		denyOnChannel(t, f, f.vch.ID, 2, store.ConnectVoice)
		b := f.dial("2", "bob")
		b.send(TypeChannelJoin, ChannelJoinData{ChannelID: strconv.FormatInt(f.vch.ID, 10)})
		wantError(t, b, ErrCodeForbidden)
	})
	t.Run("文本频道", func(t *testing.T) {
		f := newChatFixture(t)
		a := f.dial("1", "alice")
		a.send(TypeChannelJoin, ChannelJoinData{ChannelID: strconv.FormatInt(f.ch.ID, 10)})
		wantError(t, a, ErrCodeForbidden)
	})
	t.Run("频道不存在", func(t *testing.T) {
		f := newChatFixture(t)
		a := f.dial("1", "alice")
		a.send(TypeChannelJoin, ChannelJoinData{ChannelID: "4242424242"})
		wantError(t, a, ErrCodeNotFound)
	})
}

// TestChatSendPerm 发送权限门：无 VIEW / 无 SEND → forbidden；语音频道 → bad_message；
// 正常文字频道通过。
func TestChatSendPerm(t *testing.T) {
	t.Run("无 ViewChannel", func(t *testing.T) {
		f := newChatFixture(t)
		denyOnChannel(t, f, f.ch.ID, 2, store.ViewChannel)
		b := f.dial("2", "bob")
		b.recvUntil(TypeHello)
		b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "hi"})
		wantError(t, b, ErrCodeForbidden)
	})
	t.Run("无 SendMessages", func(t *testing.T) {
		f := newChatFixture(t)
		denyOnChannel(t, f, f.ch.ID, 2, store.SendMessages)
		b := f.dial("2", "bob")
		b.recvUntil(TypeHello)
		b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "hi"})
		wantError(t, b, ErrCodeForbidden)
	})
	t.Run("语音频道", func(t *testing.T) {
		f := newChatFixture(t)
		a := f.dial("1", "alice")
		a.recvUntil(TypeHello)
		// 成员发文字到语音频道 → bad_message（#35 B3：聊天只进文字频道）。
		a.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.vch.ID, 10), Type: "text", Content: "hi"})
		wantError(t, a, ErrCodeBadMessage)
	})
	t.Run("正常文字频道", func(t *testing.T) {
		f := newChatFixture(t)
		a := f.dial("1", "alice")
		a.recvUntil(TypeHello)
		a.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "ok"})
		env := a.recvUntil(TypeChatMessage)
		if env.Type != TypeChatMessage {
			t.Fatalf("应广播 chat.message，实际 %q", env.Type)
		}
	})
}

// TestChatMessageFilteredByView chat.message 逐收件人过滤：无 VIEW 的成员不进事件环、
// 不收实时投递；有 VIEW 的照常收到。
func TestChatMessageFilteredByView(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	// bob 个人 deny ViewChannel；mallory（uid 9）保持可见。
	denyOnChannel(t, f, f.ch.ID, 2, store.ViewChannel)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	m := f.dial("9", "mallory")
	for _, c := range []*wsClient{a, b, m} {
		c.recvUntil(TypeHello)
	}

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "过滤广播"})

	// 可见者收到。
	for _, c := range []*wsClient{a, m} {
		env := c.recvUntil(TypeChatMessage)
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "过滤广播" {
			t.Fatalf("content = %q", msg.Content)
		}
	}
	// 无 VIEW 的 bob 什么都收不到。
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, data, err := b.conn.Read(ctx); err == nil {
		t.Fatalf("无 VIEW 成员不应收到广播，实际 %s", data)
	}
}

// TestChatBroadcastSkipsFailedRecipient 单个收件人权限查询失败只跳过该人，
// 不能把整条消息拖成 500，也不影响其他收件人。
func TestChatBroadcastSkipsFailedRecipient(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	// mallory（uid 9）查询故障；其余放行。
	f.hub.authz = &stubAuthz{fail: map[int64]bool{9: true}}

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	m := f.dial("9", "mallory")
	for _, c := range []*wsClient{a, b, m} {
		c.recvUntil(TypeHello)
	}

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "故障跳过"})

	// 发送者与正常收件人都收到（发送失败会走 error 而非 chat.message）。
	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeChatMessage)
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "故障跳过" {
			t.Fatalf("content = %q", msg.Content)
		}
	}
	// 故障收件人被跳过。
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, data, err := m.conn.Read(ctx); err == nil {
		t.Fatalf("故障收件人应被跳过，实际 %s", data)
	}
}
