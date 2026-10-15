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

// testAuthenticator token "1"→(1,alice)、"2"→(2,bob)、"9"→(9,mallory)。
func testAuthenticator(token string) (string, string, error) {
	switch token {
	case "1":
		return "1", "alice", nil
	case "2":
		return "2", "bob", nil
	case "9":
		return "9", "mallory", nil
	}
	return "", "", ErrUnauthorized
}

// chatFixture 一套 hub + 双客户端脚手架：store 里有文本频道 ch（成员 alice/bob）。
type chatFixture struct {
	t   *testing.T
	hub *Hub
	st  *store.Mem
	srv *httptest.Server
	ch  *store.Channel
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	st := store.NewMem()
	ctx := context.Background()

	// 用户（ID 由 mem 自增，这里假定 1、2；下方断言兜底）。
	u1 := &store.User{Email: "alice@x", Nickname: "alice", Tag: "0001", PasswordHash: "x"}
	u2 := &store.User{Email: "bob@x", Nickname: "bob", Tag: "0002", PasswordHash: "x"}
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
	for _, uid := range []int64{u1.ID, u2.ID} {
		if err := st.AddMember(ctx, &store.ServerMember{ServerID: srvRec.ID, UserID: uid, Role: store.RoleMember}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	ch := &store.Channel{ServerID: srvRec.ID, Name: "general", Type: store.ChannelText}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	inst, err := sfu.New(sfu.Config{UDPPort: 0, MaxMembers: 10, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })

	hub := NewHub(inst, testAuthenticator, log.New(io.Discard, "", 0), 0, ChatDeps{
		Store:  st,
		Limits: auth.NewRateLimit(st),
	})
	hs := httptest.NewServer(newServeMux(hub))
	t.Cleanup(hs.Close)

	return &chatFixture{t: t, hub: hub, st: st, srv: hs, ch: ch}
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
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	// 各自吃掉 channel.joined（含对方加入后的第二份广播）。
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Content: "你好 bob"})

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
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)

	over := strings.Repeat("字", 2001)
	for _, content := range []string{"", over} {
		a.send(TypeChatSend, ChatSendData{ChannelID: chID, Content: content})
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
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)

	for i := 0; i < 20; i++ {
		a.send(TypeChatSend, ChatSendData{ChannelID: chID, Content: "msg"})
		if env := a.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("第 %d 条未广播", i+1)
		}
	}
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Content: "overflow"})
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
	chID := strconv.FormatInt(f.ch.ID, 10)

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
	chID := strconv.FormatInt(f.ch.ID, 10)

	mallory := f.dial("9", "mallory") // 未 join、非成员
	mallory.send(TypeChatSend, ChatSendData{ChannelID: chID, Content: "hi"})
	env := mallory.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeForbidden {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeForbidden)
	}
}
