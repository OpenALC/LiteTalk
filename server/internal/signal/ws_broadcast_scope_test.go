package signal

// ws_broadcast_scope_test.go P1-7：WS 实时广播按连接主体 scope 过滤。
// 令牌（pat/bot）连接与断线续传同口径（replayChannelVisible）：缺读位的令牌连接
// 收不到频道内容；不可见时事件环写入与实时投递一起跳过，seq 保持连续；
// presence.update 降级为不带频道键的版本。

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// newScopeHub 在 botHub 上换一个给令牌连接补 Dev/Sid 的 authenticator：生产里令牌
// 连接没有 sid，也就没有事件环，seq 连续性无从断言；这里给每枚令牌一个独立的 sid
// 和固定的设备类型（与 JWT 的 desktop 设备区分，避免同 uid+dev 互相顶替）。
func newScopeHub(t *testing.T) *botHub {
	t.Helper()
	f := newBotHub(t)
	f.hub.auth = func(raw string) (Identity, error) {
		if strings.HasPrefix(raw, token.PlainPrefix) {
			p, err := f.svc.Verify(context.Background(), raw)
			if err != nil {
				return Identity{}, ErrUnauthorized
			}
			return Identity{
				UserID:           strconv.FormatInt(p.UserID, 10),
				Username:         p.Nickname,
				Dev:              "api",
				Sid:              tokenTestSid(p.TokenID),
				SkipSessionCheck: true,
			}, nil
		}
		return testAuthenticator(raw)
	}
	return f
}

// tokenTestSid 令牌连接在测试里的会话 ID（每枚令牌一个环）。
func tokenTestSid(tokenID int64) string { return "tok-" + strconv.FormatInt(tokenID, 10) }

// issueUserToken 给既有用户签发令牌，返回 (明文, 令牌行 id)。
func (f *botHub) issueUserToken(t *testing.T, uid int64, kind string, scopes []string) (string, int64) {
	t.Helper()
	p, err := token.NewPlain(kind)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	row := &store.APIToken{
		UserID: uid, Name: "p17", Prefix: p.Prefix, SecretHash: p.Hash,
		Kind: kind, Scopes: strings.Join(scopes, " "),
	}
	if err := f.st.CreateAPIToken(context.Background(), row); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return p.Token, row.ID
}

// expectNoTypes 限时窗口内断言没有出现指定类型的信封。
func expectNoTypes(t *testing.T, p *pump, d time.Duration, types ...string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return
		}
		env, ok := p.within(rem)
		if !ok {
			return
		}
		for _, ty := range types {
			if env.Type == ty {
				t.Fatalf("不应收到 %s：%s", ty, env.Data)
			}
		}
	}
}

// TestWSBroadcastTokenScopeFilter 缺读位的令牌连接收不到频道内容（chat.message /
// channel.unread / message.edited / message.deleted），读位令牌与 JWT 连接照常。
// 服务器文字频道与私聊两种场景。
func TestWSBroadcastTokenScopeFilter(t *testing.T) {
	cases := []struct {
		name    string
		scopes  []string
		visible bool
	}{
		{"guild.manage", []string{token.ScopeGuildManage}, false},
		{"admin.read", []string{token.ScopeAdminRead}, false},
		{"messages.read", []string{token.ScopeMessagesRead}, true},
	}
	for _, scene := range []string{"server", "conversation"} {
		for _, tc := range cases {
			t.Run(scene+"/"+tc.name, func(t *testing.T) {
				f := newScopeHub(t)
				chID := strconv.FormatInt(f.ch.ID, 10)
				if scene == "conversation" {
					conv := &store.Channel{Scope: store.ChannelScopeDirect}
					if err := f.st.CreateConversation(context.Background(), conv, []int64{1, 2}); err != nil {
						t.Fatalf("CreateConversation: %v", err)
					}
					chID = strconv.FormatInt(conv.ID, 10)
				}
				plain, tid := f.issueUserToken(t, 2, token.KindPAT, tc.scopes)
				tok := newPump(t, f.dialBot(t, plain))
				jwtB := newPump(t, f.dial("2", "bob"))
				alice := f.dial("1", "alice")

				alice.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "p17-secret"})
				jwtB.until(TypeChatMessage)
				jwtB.until(TypeChannelUnread)
				f.hub.BroadcastMessageEdited([]string{"2"}, chID, "1", "edited", time.Now().UnixMilli(), nil)
				jwtB.until(TypeMessageEdited)
				f.hub.BroadcastMessageDeleted([]string{"2"}, chID, "1")
				jwtB.until(TypeMessageDeleted)

				sid := tokenTestSid(tid)
				if tc.visible {
					for _, ty := range []string{TypeChatMessage, TypeChannelUnread, TypeMessageEdited, TypeMessageDeleted} {
						tok.until(ty)
					}
					return
				}
				// 环写入在所有投递之前完成：JWT 连接收到最后一条即代表环状态已定。
				if got := f.hub.evlog.LatestSeq(sid); got != 0 {
					t.Fatalf("不可见令牌连接的事件环不应有写入，LatestSeq = %d", got)
				}
				expectNoTypes(t, tok, 300*time.Millisecond,
					TypeChatMessage, TypeChannelUnread, TypeMessageEdited, TypeMessageDeleted)
			})
		}
	}
}

// TestWSBroadcastTokenSeqContiguous 同一用户令牌连接 + JWT 连接：令牌连接两次跳过
// 不可见的 chat.message / channel.unread 后收到 conversation.changed，信封 seq
// 必须是 1、2（环里没有被跳过事件占位），否则客户端 tracker 会把后续事件压进乱序缓冲。
func TestWSBroadcastTokenSeqContiguous(t *testing.T) {
	f := newScopeHub(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	plain, tid := f.issueUserToken(t, 2, token.KindPAT, []string{token.ScopeGuildManage})
	tok := newPump(t, f.dialBot(t, plain))
	jwtB := newPump(t, f.dial("2", "bob"))
	alice := f.dial("1", "alice")
	sid := tokenTestSid(tid)

	for round := int64(1); round <= 2; round++ {
		alice.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "hidden"})
		jwtB.until(TypeChannelUnread)
		f.hub.BroadcastConversationChanged([]string{"2"}, "42", "created")
		env := tok.until(TypeConversationChanged)
		if env.Seq != round {
			t.Fatalf("第 %d 轮 conversation.changed seq = %d，want %d（被跳过的事件不得占用 seq）", round, env.Seq, round)
		}
	}
	events, reason := f.hub.evlog.Replay(sid, 1)
	if reason != "" || len(events) != 2 {
		t.Fatalf("令牌连接事件环 = %d 条 reason=%q，want 2 条 conversation.changed", len(events), reason)
	}
	for i, ev := range events {
		if ev.Type != TypeConversationChanged || ev.Seq != int64(i+1) {
			t.Fatalf("环内事件 %d = %s seq=%d", i, ev.Type, ev.Seq)
		}
	}
}

// TestWSBroadcastTokenPresenceDowngrade 好友进语音房：看不到该频道的令牌连接收到不带
// channel_id/server_id 的 presence.update，且写入自己的环、seq 连续；JWT 连接收到完整字段。
func TestWSBroadcastTokenPresenceDowngrade(t *testing.T) {
	f := newScopeHub(t)
	f.withPresence()
	makeFriends(t, f.chatFixture, 1, 2, store.FriendAccepted)
	chID, srvID := vchIDs(f.chatFixture)
	plain, tid := f.issueUserToken(t, 2, token.KindPAT, []string{token.ScopeGuildManage})
	tok := newPump(t, f.dialBot(t, plain))
	jwtB := newPump(t, f.dial("2", "bob"))

	alice := f.dial("1", "alice")
	online := tok.until(TypePresenceUpdate) // 上线推送（无频道维度，不过滤）
	jwtB.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)

	decodeChan(t, jwtB.until(TypePresenceUpdate), chID, srvID)
	env := tok.until(TypePresenceUpdate)
	assertNoChanKeys(t, env)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || !d.Online || d.UserID != "1" {
		t.Fatalf("降级载荷 = %+v err=%v", d, err)
	}
	if env.Seq != online.Seq+1 {
		t.Fatalf("降级事件 seq = %d，want %d（连续）", env.Seq, online.Seq+1)
	}
	events, _ := f.hub.evlog.Replay(tokenTestSid(tid), 1)
	if len(events) != 2 {
		t.Fatalf("令牌连接事件环 = %d 条，want 2", len(events))
	}
	assertNoChanKeys(t, Envelope{Data: events[1].Payload})
}

// TestTokenConnVisibleFailClosed 判定出错/不可判定一律不可见：dev 内置房间（非数字
// 频道 id）对令牌连接不可见；JWT/无主体连接不属于令牌连接。
func TestTokenConnVisibleFailClosed(t *testing.T) {
	f := newChatFixture(t)
	p := &token.Principal{Claims: &auth.Claims{}, Kind: token.KindPAT, Scopes: []string{token.ScopeMessagesRead}}
	c := &client{userID: "2", principal: p}
	if f.hub.tokenConnVisible(c, "dev-room", map[string]bool{}) {
		t.Fatal("非数字频道对令牌连接应不可见")
	}
	if !isTokenConn(c) {
		t.Fatal("pat 主体应判为令牌连接")
	}
	if isTokenConn(&client{userID: "2"}) || isTokenConn(&client{principal: &token.Principal{Claims: &auth.Claims{}, Kind: token.KindJWT}}) {
		t.Fatal("无主体/JWT 主体不应判为令牌连接")
	}
}
