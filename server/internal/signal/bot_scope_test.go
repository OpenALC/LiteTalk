package signal

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/authz"
	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
	"github.com/coder/websocket"
)

// botHub 夹具：hub 注入真 token.Service（与生产装配同构），authenticator 处理
// ltk_ 前缀——校验失败 401、成功产出 SkipSessionCheck 的 Identity。
type botHub struct {
	*chatFixture
	svc *token.Service
}

func newBotHub(t *testing.T) *botHub {
	t.Helper()
	base := newChatFixture(t)
	svc := token.NewService(base.st, log.New(io.Discard, "", 0))
	inst, err := sfu.New(sfu.Config{UDPPort: 0, MaxMembers: 10, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	authn := func(raw string) (Identity, error) {
		if strings.HasPrefix(raw, token.PlainPrefix) {
			p, verr := svc.Verify(context.Background(), raw)
			if verr != nil {
				return Identity{}, ErrUnauthorized
			}
			return Identity{
				UserID:           strconv.FormatInt(p.UserID, 10),
				Username:         p.Nickname,
				SkipSessionCheck: true,
			}, nil
		}
		return testAuthenticator(raw)
	}
	hub := NewHub(inst, authn, nil, log.New(io.Discard, "", 0), 0, ChatDeps{
		Store:  base.st,
		Limits: auth.NewRateLimit(base.st),
		Authz:  authz.New(base.st, authz.Options{}),
		Tokens: svc,
	})
	hs := httptest.NewServer(newServeMux(hub))
	t.Cleanup(hs.Close)
	base.hub = hub
	base.srv = hs
	return &botHub{chatFixture: base, svc: svc}
}

// botSeq 夹具内的 bot 序号：同测试多次签发需唯一 email/nickname（唯一索引）。
var botSeq atomic.Int64

// issueBotToken 建 bot 用户（加入服务器为成员）并签发 kind=bot 令牌，
// 返回 (明文, bot uid, 令牌行 id)。
func (f *botHub) issueBotToken(t *testing.T, scopes []string) (plain string, botUID, tokenID int64) {
	t.Helper()
	ctx := context.Background()
	seq := botSeq.Add(1)
	bot := &store.User{
		Email:    "bot" + strconv.FormatInt(seq, 10) + "@bots.invalid",
		Nickname: "helper-bot-" + strconv.FormatInt(seq, 10),
		Tag:      fmt.Sprintf("%04d", 9000+seq%1000),
		IsBot:    true,
	}
	if err := f.st.CreateUser(ctx, bot); err != nil {
		t.Fatalf("CreateUser bot: %v", err)
	}
	if err := f.st.AddMember(ctx, &store.ServerMember{ServerID: f.ch.ServerID, UserID: bot.ID, Role: store.RoleMember}); err != nil {
		t.Fatalf("AddMember bot: %v", err)
	}
	// 夹具绕过 REST 直写成员表：必须手动失效 authz 的按服缓存（生产由 REST
	// 加成员路径做），否则先建的快照里没有后加的 bot，判定全是非成员。
	f.hub.authz.InvalidateServer(f.ch.ServerID)
	p, err := token.NewPlain(token.KindBot)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	row := &store.APIToken{
		UserID:     bot.ID,
		Name:       "test",
		Prefix:     p.Prefix,
		SecretHash: p.Hash,
		Kind:       token.KindBot,
		Scopes:     strings.Join(scopes, " "),
	}
	if err := f.st.CreateAPIToken(ctx, row); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return p.Token, bot.ID, row.ID
}

// dialBot 以 bot 令牌连 WS（同一条 /ws 路径，token 即 api_token）。
func (f *botHub) dialBot(t *testing.T, plain string) *wsClient {
	t.Helper()
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?token=" + plain
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial bot: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	c := &wsClient{t: t, conn: conn, user: "bot"}
	c.awaitHello()
	return c
}

// TestBotScopeNarrowsChatSend 核心验收（#51）：bot 只给 messages.read 时 chat.send
// 被拒；换 messages.read+messages.write 的令牌后发送成功；只给 voice.connect 的
// 令牌同样发不了。scope 收窄与 REST 汇聚到 authz.CanInServer 的同一交集点，
// 没有给 bot 单开的平行分支。
func TestBotScopeNarrowsChatSend(t *testing.T) {
	f := newBotHub(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	readToken, _, _ := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	bot := f.dialBot(t, readToken)
	bot.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "hello"})
	wantError(t, bot, ErrCodeForbidden)

	// 同 bot 换上有 messages.write 的令牌：放行并广播（角色成员权限 ∩ scope 后仍够）。
	writeToken, _, _ := f.issueBotToken(t, []string{token.ScopeMessagesRead, token.ScopeMessagesWrite})
	bot2 := f.dialBot(t, writeToken)
	bot2.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "bot 在线"})
	env := bot2.recvUntil(TypeChatMessage)
	var msg ChatMessageData
	if err := env.Decode(&msg); err != nil {
		t.Fatalf("decode chat.message: %v", err)
	}
	if msg.Content != "bot 在线" {
		t.Fatalf("content = %q", msg.Content)
	}

	// voice.connect 不含 SendMessages 位，同样发不了文字消息。
	voiceToken, _, _ := f.issueBotToken(t, []string{token.ScopeVoiceConnect})
	bot3 := f.dialBot(t, voiceToken)
	bot3.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "nope"})
	wantError(t, bot3, ErrCodeForbidden)
}

// TestBotScopeNarrowsChannelJoin 只给 messages.read 的 bot 进不了语音频道
// （voice.connect 缺失 → ConnectVoice 位被交集掉）；给 voice.connect 后放行。
func TestBotScopeNarrowsChannelJoin(t *testing.T) {
	f := newBotHub(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	readToken, _, _ := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	bot := f.dialBot(t, readToken)
	bot.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, bot, ErrCodeForbidden)

	voiceToken, _, _ := f.issueBotToken(t, []string{token.ScopeVoiceConnect})
	bot2 := f.dialBot(t, voiceToken)
	bot2.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	env := bot2.recvUntil(TypeChannelJoined)
	var joined ChannelJoinedData
	if err := env.Decode(&joined); err != nil {
		t.Fatalf("decode channel.joined: %v", err)
	}
	if joined.ChannelID != vchID {
		t.Fatalf("channel_id = %q, want %q", joined.ChannelID, vchID)
	}
}

// TestBotTokenRevokedWSRejected 令牌吊销立即生效：吊销前可握手，吊销后同令牌
// 握手 401（校验路径无 Principal 缓存，T2 机制；与 REST 吊销同一落库路径）。
func TestBotTokenRevokedWSRejected(t *testing.T) {
	f := newBotHub(t)
	plain, _, tokenID := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	bot := f.dialBot(t, plain)
	bot.recvUntil(TypeHello)

	if err := f.st.RevokeAPIToken(context.Background(), tokenID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}

	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?token=" + plain
	if _, resp, err := websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("吊销后握手应失败")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("吊销后握手状态码 = %v, want 401", resp)
	}
}

// TestUIDIsBot presence 广播过滤的判定单元：bot 命中、普通用户与 dev uid 不命中。
func TestUIDIsBot(t *testing.T) {
	f := newBotHub(t)
	ctx := context.Background()
	bot := &store.User{Email: "bot2@bots.invalid", Nickname: "quiet-bot", Tag: "9002", IsBot: true}
	if err := f.st.CreateUser(ctx, bot); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if !f.hub.uidIsBot(strconv.FormatInt(bot.ID, 10)) {
		t.Fatal("bot uid 应命中")
	}
	if f.hub.uidIsBot("1") {
		t.Fatal("普通用户不应命中")
	}
	if f.hub.uidIsBot("dev-alice") {
		t.Fatal("非数字 uid 不应命中")
	}
}

// TestBotScopeNarrowsConversationSend 会话频道的 scope 门（P1-3）：会话分支只查
// 成员表（成员即全权），曾是 scope 判定的绕过点——messages.read 的 bot 在自己
// 是成员的私聊里 chat.send 也被拒；换 messages.write 的令牌放行并广播给全部成员。
func TestBotScopeNarrowsConversationSend(t *testing.T) {
	f := newBotHub(t)
	ctx := context.Background()

	readPlain, botUID, _ := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	// 会话成员 = alice(1) + bot；bot 已是服务器成员（夹具），会话不看服务器身份。
	conv := &store.Channel{Scope: store.ChannelScopeDirect}
	if err := f.st.CreateConversation(ctx, conv, []int64{1, botUID}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := strconv.FormatInt(conv.ID, 10)

	// 同一 bot 的写令牌：issueBotToken 每次建新 bot（会话成员随之变化），
	// 这里给既有 bot 直落一行 kind=bot 令牌。
	p, err := token.NewPlain(token.KindBot)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateAPIToken(ctx, &store.APIToken{UserID: botUID, Name: "write",
		Prefix: p.Prefix, SecretHash: p.Hash, Kind: token.KindBot, Scopes: token.ScopeMessagesWrite}); err != nil {
		t.Fatal(err)
	}

	// messages.read：成员但无写 scope → forbidden（修前消息会落库并广播）。
	bot := f.dialBot(t, readPlain)
	bot.send(TypeChatSend, ChatSendData{ChannelID: convID, Type: "text", Content: "越权"})
	wantError(t, bot, ErrCodeForbidden)

	// messages.write：放行，广播目标是全部会话成员（alice 也在内）。
	a := f.dial("1", "alice")
	bot2 := f.dialBot(t, p.Token)
	bot2.send(TypeChatSend, ChatSendData{ChannelID: convID, Type: "text", Content: "bot 受权"})
	for _, c := range []*wsClient{bot2, a} {
		env := c.recvUntil(TypeChatMessage)
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "bot 受权" || msg.ChannelID != convID {
			t.Fatalf("%s 收到的会话载荷不符: %+v", c.user, msg)
		}
	}
}
