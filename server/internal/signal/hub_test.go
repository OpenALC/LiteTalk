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
}

func (f *chatFixture) dial(token, name string) *wsClient {
	f.t.Helper()
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?token=" + token
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		f.t.Fatalf("dial %s: %v", name, err)
	}
	f.t.Cleanup(func() { conn.CloseNow() })
	return &wsClient{t: f.t, conn: conn, user: name}
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

// recvUntil 读到指定类型为止（跳过 hello/joined 等中间消息），超时 5s。
func (c *wsClient) recvUntil(want string) Envelope {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			c.t.Fatalf("recvUntil %s (user=%s): %v", want, c.user, err)
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type == want {
			return env
		}
	}
	c.t.Fatalf("recvUntil %s: 超时（user=%s）", want, c.user)
	return Envelope{}
}

// ---- 测试用例 ----

// TestChatBroadcast 双客户端同房：A 发 chat.send → 双方收 chat.message（内容/用户正确），
// 且消息落库可查。
func TestChatBroadcast(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	// 各自吃掉 channel.joined（含对方加入后的第二份广播）。
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

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
	msgs, err := f.st.MessagesBefore(context.Background(), f.vch.ID, 0, 10)
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
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)

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
	msgs, err := f.st.MessagesBefore(context.Background(), f.vch.ID, 0, 10)
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
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)

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
