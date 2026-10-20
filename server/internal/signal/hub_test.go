package signal

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/coder/websocket"
)

// ---- 测试脚手架 ----

// testAuthenticator token 形如 "1"（默认 dev=desktop、固定 sid）或 "1:<dev>:<sid>"。
// uid 映射："1"→alice、"2"→bob、"9"→mallory、"8"→eve。
// mallory 在 fixture 里是服务器成员，eve 不是（进房权限测试用）。
func testAuthenticator(token string) (Identity, error) {
	parts := strings.SplitN(token, ":", 3)
	var name string
	switch parts[0] {
	case "1":
		name = "alice"
	case "2":
		name = "bob"
	case "9":
		name = "mallory"
	case "8":
		name = "eve"
	default:
		return Identity{}, ErrUnauthorized
	}
	id := Identity{
		UserID:   parts[0],
		Username: name,
		Dev:      "desktop",
		Sid:      defaultTestSid(parts[0]),
	}
	if len(parts) > 1 && parts[1] != "" {
		id.Dev = parts[1]
	}
	if len(parts) > 2 {
		id.Sid = parts[2]
	}
	return id, nil
}

// defaultTestSid 每个 uid 的默认 sid（32 字符；未指定会话的旧用例用）。
func defaultTestSid(uid string) string { return strings.Repeat("s"+uid, 32/len("s"+uid)) }

// chatFixture 一套 hub + 双客户端脚手架：store 里有文本频道 ch、语音频道 vch（成员 alice/bob）。
type chatFixture struct {
	t   *testing.T
	hub *Hub
	st  *store.Mem
	srv *httptest.Server
	ch  *store.Channel
	vch *store.Channel
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	st := store.NewMem()
	ctx := context.Background()

	// 用户 ID 预置 1、2（testAuthenticator 的 token→uid 映射按这两个值走）。
	// Snowflake 赋值后显式预置不再被覆盖，夹具保持确定性。
	u1 := &store.User{ID: 1, Email: "alice@x", Nickname: "alice", Tag: "0001", PasswordHash: "x"}
	u2 := &store.User{ID: 2, Email: "bob@x", Nickname: "bob", Tag: "0002", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u1); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.CreateUser(ctx, u2); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	srvRec := &store.Server{Name: "s", OwnerID: u1.ID, IconName: "hash", IconColor: "#fff"}
	if err := st.CreateServer(ctx, srvRec); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	for _, uid := range []int64{u1.ID, u2.ID, 9} { // 9 = mallory：成员（进房权限用例另行用非成员 eve）
		if err := st.AddMember(ctx, &store.ServerMember{ServerID: srvRec.ID, UserID: uid, Role: store.RoleMember}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	ch := &store.Channel{ServerID: srvRec.ID, Name: "general", Type: store.ChannelText}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	vch := &store.Channel{ServerID: srvRec.ID, Name: "lobby", Type: store.ChannelVoice}
	if err := st.CreateChannel(ctx, vch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	hub, hs := newHubServer(t, st, false)

	return &chatFixture{t: t, hub: hub, st: st, srv: hs, ch: ch, vch: vch}
}

// newHubServer 用给定 store 搭一套 hub + httptest 服务；dev 控制非数字内置房间放行。
func newHubServer(t *testing.T, st *store.Mem, dev bool) (*Hub, *httptest.Server) {
	t.Helper()
	inst, err := sfu.New(sfu.Config{UDPPort: 0, MaxMembers: 10, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })

	hub := NewHub(inst, testAuthenticator, nil, log.New(io.Discard, "", 0), 0, ChatDeps{
		Store:  st,
		Limits: auth.NewRateLimit(st),
		Dev:    dev,
	})
	hs := httptest.NewServer(newServeMux(hub))
	t.Cleanup(hs.Close)
	return hub, hs
}

func newServeMux(h *Hub) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	return mux
}

// ---- WS 测试客户端 ----

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	user string

	// pending 预读缓冲：dial 为等服务端 register 完成会先读到 hello（register
	// 先于 hello 下发，见 hub.go ServeWS），途中若夹带其它信封则缓存于此，
	// recvUntil 先排空缓冲再读连接。没有它，"dial 完立刻 send" 会撞上
	// 服务端尚未 register 的窗口——广播直接跳过该连接，对端永远等不到消息
	// （CI 上表现为间歇 5s 超时，TestGatewayResumeTrailing 踩过）。
	pending []Envelope
}

func (f *chatFixture) dial(token, name string) *wsClient {
	f.t.Helper()
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?token=" + token
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		f.t.Fatalf("dial %s: %v", name, err)
	}
	f.t.Cleanup(func() { conn.CloseNow() })
	c := &wsClient{t: f.t, conn: conn, user: name}
	// 等到 hello：服务端 register + 事件环 Bind 都先于它下发（hub.go:235-245），
	// 收到即保证该连接已被房间广播覆盖。显式 recvUntil(TypeHello) 的测试不受
	// 影响——hello 进了 pending，recvUntil 会先取到它。
	c.awaitHello()
	return c
}

// awaitHello 读到 hello 为止，途中信封进 pending。超时 5s（与 recvUntil 一致）。
func (c *wsClient) awaitHello() {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env := c.readOne()
		if env.Type == TypeHello {
			c.pending = append(c.pending, env)
			return
		}
		c.pending = append(c.pending, env)
	}
	c.t.Fatalf("awaitHello: 5s 内未收到 hello（user=%s）", c.user)
}

func (c *wsClient) send(msgType string, data any) {
	c.t.Helper()
	raw, err := MarshalEnvelope(msgType, data)
	if err != nil {
		c.t.Fatalf("marshal %s: %v", msgType, err)
	}
	if err := c.conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		c.t.Fatalf("write %s: %v", msgType, err)
	}
}

// readOne 读一个信封（超时 5s）。awaitHello 与 recvUntil 共用。
func (c *wsClient) readOne() Envelope {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("readOne (user=%s): %v", c.user, err)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		c.t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

// recvUntil 读到指定类型为止（跳过 hello/joined 等中间消息），超时 5s。
// 先排空 dial 预读进 pending 的信封，再读连接。
func (c *wsClient) recvUntil(want string) Envelope {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var env Envelope
		if len(c.pending) > 0 {
			env, c.pending = c.pending[0], c.pending[1:]
		} else {
			env = c.readOne()
		}
		if env.Type == want {
			return env
		}
	}
	c.t.Fatalf("recvUntil %s: 超时（user=%s）", want, c.user)
	return Envelope{}
}

// ---- 测试用例 ----

// TestChatBroadcast 文字频道：A 发 chat.send → 服务器成员收 chat.message（内容/用户正确），
// 且消息落库可查。#35 B3 起语音频道拒收文字（bad_message），聊天场景走文字频道。
func TestChatBroadcast(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	for _, c := range []*wsClient{a, b} {
		c.recvUntil(TypeHello)
	}

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "你好 bob"})

	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeChatMessage)
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "你好 bob" {
			t.Fatalf("content = %q", msg.Content)
		}
		if msg.ChannelID != chID {
			t.Fatalf("channel_id = %q, want %q", msg.ChannelID, chID)
		}
		if msg.User.ID != "1" || msg.User.Nickname != "alice" {
			t.Fatalf("user = %+v, want id=1/alice", msg.User)
		}
		if msg.ID == "" || msg.TS <= 0 {
			t.Fatalf("id/ts 未填充: %+v", msg)
		}
	}

	// 落库可查。
	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "你好 bob" || msgs[0].UserID != 1 {
		t.Fatalf("落库不一致: %+v", msgs)
	}
}

// TestChatBadContent 超长（2001 字）与空内容拒收（bad_message）。
func TestChatBadContent(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)

	over := strings.Repeat("字", 2001)
	for _, content := range []string{"", over} {
		a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: content})
		env := a.recvUntil(TypeError)
		var e ErrorData
		if err := env.Decode(&e); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if e.Code != ErrCodeBadMessage {
			t.Fatalf("code = %q, want %q", e.Code, ErrCodeBadMessage)
		}
	}

	// 拒收后不落库。
	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("非法内容不应落库: %+v", msgs)
	}
}

// TestChatRateLimit 20 条/分：第 21 条触发 rate_limited。
func TestChatRateLimit(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)

	for i := 0; i < 20; i++ {
		a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "msg"})
		if env := a.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("第 %d 条未广播", i+1)
		}
	}
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "overflow"})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeRateLimited {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeRateLimited)
	}
}

// TestVoiceRequestState 定向投递：A 请求 B 闭麦 → B 收 voice.state_request（含来源），
// A 自己收不到；目标不在频道 → A 收 not_in_channel。
func TestVoiceRequestState(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	mute := true
	a.send(TypeVoiceRequestState, VoiceRequestStateData{ChannelID: chID, TargetID: "2", Mute: &mute})

	env := b.recvUntil(TypeVoiceStateRequest)
	var req VoiceStateRequestData
	if err := env.Decode(&req); err != nil {
		t.Fatalf("decode voice.state_request: %v", err)
	}
	if req.From.ID != "1" || req.From.Nickname != "alice" {
		t.Fatalf("from = %+v, want alice", req.From)
	}
	if req.Mute == nil || !*req.Mute {
		t.Fatalf("mute = %+v, want true", req.Mute)
	}

	// 目标不在频道（mallory 未加入）→ 发送者收 not_in_channel。
	m := f.dial("9", "mallory")
	m.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	m.recvUntil(TypeChannelJoined)
	m.send(TypeChannelLeave, ChannelLeaveData{})
	m.send(TypeVoiceRequestState, VoiceRequestStateData{ChannelID: chID, TargetID: "2", Mute: &mute})
	env = m.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeNotInChannel {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeNotInChannel)
	}
}

// TestChatForbidden 既不在房间、也不是频道成员 → forbidden。
func TestChatForbidden(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	eve := f.dial("8", "eve") // 未 join、非成员
	eve.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "hi"})
	env := eve.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeForbidden {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeForbidden)
	}
}

// TestChannelJoinForbidden 非服务器成员加入语音频道 → forbidden（P0 S1）。
func TestChannelJoinForbidden(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	eve := f.dial("8", "eve") // 非该服务器成员
	eve.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	env := eve.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeForbidden {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeForbidden)
	}
}

// TestChannelJoinTextForbidden 成员加入文本频道 → forbidden（只能进语音频道）。
func TestChannelJoinTextForbidden(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeForbidden {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeForbidden)
	}
}

// TestChannelJoinNotFound 加入不存在的数字频道 → not_found。
func TestChannelJoinNotFound(t *testing.T) {
	f := newChatFixture(t)

	a := f.dial("1", "alice")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: "999999"})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeNotFound {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeNotFound)
	}
}

// TestChannelJoinDevRoom 非数字内置房间：生产 hub 拒绝（not_found），dev hub 放行。
func TestChannelJoinDevRoom(t *testing.T) {
	f := newChatFixture(t)

	// 生产（Dev=false）：内置房间不存在于 store → not_found。
	a := f.dial("1", "alice")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: "builtin-lobby"})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeNotFound {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeNotFound)
	}

	// dev（Dev=true）：放行并正常下发 channel.joined。
	_, hs := newHubServer(f.t, f.st, true)
	devClient := &wsClient{t: f.t, user: "alice"}
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(hs.URL, "http")+"/ws?token=1", nil)
	if err != nil {
		f.t.Fatalf("dial dev hub: %v", err)
	}
	f.t.Cleanup(func() { conn.CloseNow() })
	devClient.conn = conn
	devClient.send(TypeChannelJoin, ChannelJoinData{ChannelID: "builtin-lobby"})
	got := devClient.recvUntil(TypeChannelJoined)
	if got.Type != TypeChannelJoined {
		t.Fatalf("dev 房间应能加入，实际收到 %q", got.Type)
	}
	var joined ChannelJoinedData
	if err := got.Decode(&joined); err != nil {
		t.Fatalf("decode channel.joined: %v", err)
	}
	if joined.ChannelID != "builtin-lobby" || len(joined.Members) != 1 {
		t.Fatalf("joined 数据不符: %+v", joined)
	}
}

// TestKickChannelRemovesAllMembers 频道删除收尾（#22）：房间内全部连接收到
// session.voice_taken、成员登记与房间清空，WS 连接本身保留并可重新入房。
func TestKickChannelRemovesAllMembers(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.recvUntil(TypeChannelJoined)

	f.hub.KickChannel(chID, "channel_deleted")

	// 两名成员都应收到 session.voice_taken（中间可能夹带 peer_left）。
	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeSessionVoiceTaken)
		if env.Type != TypeSessionVoiceTaken {
			t.Fatalf("%s 未收到 session.voice_taken，实际 %q", c.user, env.Type)
		}
	}
	if got := f.hub.roomMemberCount(chID); got != 0 {
		t.Fatalf("房间成员数 = %d, want 0", got)
	}
	if f.hub.roomMember(chID, "1") != nil || f.hub.roomMember(chID, "2") != nil {
		t.Fatal("成员登记应已清空")
	}
	// 只摘房间归属，不断 WS 连接。
	if got := f.hub.connCount("1"); got != 1 {
		t.Fatalf("alice 连接数 = %d, want 1", got)
	}
	if got := f.hub.connCount("2"); got != 1 {
		t.Fatalf("bob 连接数 = %d, want 1", got)
	}
	// 房间已销毁，重新入房应正常。
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	if f.hub.roomMember(chID, "1") == nil {
		t.Fatal("重新入房后 alice 应重新登记为成员")
	}
}

// TestKickChannelNoRoomIsNoop 房间不存在或为空时 KickChannel 是无操作：
// 不创建空房间、不 panic（频道/服务器删除路径会无条件调用）。
func TestKickChannelNoRoomIsNoop(t *testing.T) {
	hub, _ := newHubServer(t, store.NewMem(), false)
	hub.KickChannel("404", "channel_deleted")
	if got := hub.roomMemberCount("404"); got != 0 {
		t.Fatalf("不存在频道不应被创建房间，实际成员数 %d", got)
	}
	hub.KickChannel("404", "") // 空 reason 同样安全
}

// TestKickChannelIdentityCheck 成员表里的连接若 roomID 已不在该房（陈旧登记），
// KickChannel 不盲删该登记；正常成员仍被摘除并收到 session.voice_taken。
func TestKickChannelIdentityCheck(t *testing.T) {
	hub, _ := newHubServer(t, store.NewMem(), false)
	stale := &client{hub: hub, userID: "1", dev: "desktop", send: make(chan []byte, 4)}
	active := &client{hub: hub, userID: "2", dev: "desktop", send: make(chan []byte, 4)}
	active.setRoomID("7")
	hub.mu.Lock()
	hub.rooms["7"] = &roomState{id: "7", members: map[string]*client{
		"1": stale, // roomID 为空：与该房不一致
		"2": active,
	}}
	hub.mu.Unlock()

	hub.KickChannel("7", "channel_deleted")

	if got := hub.roomMember("7", "1"); got != stale {
		t.Fatal("陈旧登记不应被盲删（身份核对）")
	}
	if got := hub.roomMember("7", "2"); got != nil {
		t.Fatal("正常成员应被摘除")
	}
	select {
	case raw := <-active.send:
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil || env.Type != TypeSessionVoiceTaken {
			t.Fatalf("正常成员应收到 session.voice_taken，实际 %s err=%v", raw, err)
		}
	default:
		t.Fatal("正常成员应收到 session.voice_taken")
	}
	// 陈旧登记不会被摘出房间，故仍可能收到 peer_left 广播；但绝不能收到
	// session.voice_taken（那是给被摘成员的退房通知）。
drain:
	for {
		select {
		case raw := <-stale.send:
			var env Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("解码下行消息: %v", err)
			}
			if env.Type == TypeSessionVoiceTaken {
				t.Fatalf("陈旧登记不应收到 session.voice_taken，实际 %s", raw)
			}
		default:
			break drain
		}
	}
}

// TestHubShutdown 广播 server.shutdown 后连接关闭（#21）。
func TestHubShutdown(t *testing.T) {
	f := newChatFixture(t)
	c := f.dial("1", "alice")
	c.recvUntil(TypeHello)

	go f.hub.Shutdown("shutdown")

	env := c.recvUntil(TypeServerShutdown)
	var d ServerShutdownData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("decode ServerShutdownData: %v", err)
	}
	if d.Reason != "shutdown" {
		t.Fatalf("reason = %q, want shutdown", d.Reason)
	}

	// 连接应在发送窗口后被 CloseNow。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := c.conn.Read(ctx)
	if err == nil {
		t.Fatal("连接应已关闭")
	}
}

// TestHubShutdownRejectsNewAndWaitsCleanup (#21)：置位后新 WS 在鉴权/注册之前
// 返回 503；Shutdown 要等旧连接 cleanup 完成（conns 清零）才返回，而非发完
// server.shutdown 就立刻返回。等待上限在测试里放宽到 2s，避免拖慢。
func TestHubShutdownRejectsNewAndWaitsCleanup(t *testing.T) {
	f := newChatFixture(t)
	f.hub.shutdownWait = 2 * time.Second
	old := f.dial("1", "alice")
	old.recvUntil(TypeHello)

	done := make(chan struct{})
	go func() {
		f.hub.Shutdown("shutdown")
		close(done)
	}()

	old.recvUntil(TypeServerShutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := old.conn.Read(ctx); err == nil {
		t.Fatal("旧连接应已关闭")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown 未在旧连接 cleanup 后返回（#21）")
	}
	// Shutdown 返回即意味着 cleanup 的 unregister 已执行：conns 必须清零。
	if n := f.hub.connCount("1"); n != 0 {
		t.Fatalf("Shutdown 返回时仍有 %d 条连接未完成 cleanup（#21）", n)
	}

	// 关闭置位后新 WS 一律 503，响应体给出 shutting_down。
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?token=2"
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("关闭中不应接受新 WS 连接")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("新 WS 应返回 503，实际 resp=%v err=%v", resp, err)
	}
	body, _ := io.ReadAll(resp.Body)
	var e struct{ Code, Message string }
	if err := json.Unmarshal(body, &e); err != nil || e.Code != "shutting_down" {
		t.Fatalf("503 响应体 = %s err=%v，期望 code=shutting_down", body, err)
	}

	// 幂等：重复调用立即返回，不重新进入关闭流程。
	second := make(chan struct{})
	go func() {
		f.hub.Shutdown("again")
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("重复 Shutdown 未立即返回（#21 幂等）")
	}
}
