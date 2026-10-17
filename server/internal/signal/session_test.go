package signal

// 多设备会话（v1.1.0 §0）：hub 单连接索引、踢线/替换 close code、心跳僵尸清理、
// 语音单处 voice_taken、握手 sid 比对、chat 类型校验与按服务器推送。

import (
	"context"
	"io"
	"log"
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

// ---- 辅助 ----

// readCloseCode 读到连接关闭为止（允许中间还有数据帧被跳过），返回 close code。
func (c *wsClient) readCloseCode() websocket.StatusCode {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, _, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			code := websocket.CloseStatus(err)
			if code == -1 {
				c.t.Fatalf("readCloseCode(user=%s): 非关闭错误: %v", c.user, err)
			}
			return code
		}
	}
	c.t.Fatalf("readCloseCode(user=%s): 超时未关闭", c.user)
	return -1
}

// readExpectClose 期望下一次 Read 直接以 want 关闭（此前不得再有任何数据帧）。
func (c *wsClient) readExpectClose(want websocket.StatusCode) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err == nil {
		c.t.Fatalf("readExpectClose(user=%s): 期望直接关闭 %d，却收到消息 %s", c.user, want, data)
	}
	if code := websocket.CloseStatus(err); code != want {
		c.t.Fatalf("readExpectClose(user=%s): close code = %d, want %d（err=%v）", c.user, code, want, err)
	}
}

// waitConnsEmpty 轮询等待 hub 连接索引清空（cleanup 跑完）。
func waitConnsEmpty(t *testing.T, hub *Hub) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		hub.mu.Lock()
		n := len(hub.conns)
		hub.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待连接索引清空超时（残留 %d 条）", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newHubServerSess 带 SessionIndex 的 hub 脚手架（握手 sid 比对用）。
func newHubServerSess(t *testing.T, st *store.Mem, sess *auth.SessionIndex) (*Hub, *httptest.Server) {
	t.Helper()
	inst, err := sfu.New(sfu.Config{UDPPort: 0, MaxMembers: 10, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	hub := NewHub(inst, testAuthenticator, sess, log.New(io.Discard, "", 0), 0, ChatDeps{Store: st})
	hs := httptest.NewServer(newServeMux(hub))
	t.Cleanup(hs.Close)
	return hub, hs
}

func dialTo(t *testing.T, hs *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws?token=" + token
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", token, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// ---- T4：双连接互踢 / 同 sid 替换 ----

// TestSessionKickedSameDevice 同 uid+dev 新登录（异 sid）：旧连接先收
// session.kicked（reason=login_elsewhere、device_type 正确），随后 close 4001；
// 新连接顶替其索引槽位。
func TestSessionKickedSameDevice(t *testing.T) {
	f := newChatFixture(t)
	sidA, sidB := strings.Repeat("a", 32), strings.Repeat("b", 32)

	a := f.dial("1:desktop:"+sidA, "alice-A")
	a.recvUntil(TypeHello)
	b := f.dial("1:desktop:"+sidB, "alice-B")

	env := a.recvUntil(TypeSessionKicked)
	var kicked SessionKickedData
	if err := env.Decode(&kicked); err != nil {
		t.Fatalf("decode session.kicked: %v", err)
	}
	if kicked.Reason != KickReasonLoginElsewhere {
		t.Fatalf("reason = %q, want %q", kicked.Reason, KickReasonLoginElsewhere)
	}
	if kicked.DeviceType != "desktop" {
		t.Fatalf("device_type = %q, want desktop", kicked.DeviceType)
	}
	a.readExpectClose(CloseCodeNewLogin)

	// 索引槽位被新连接顶替；旧连接的 cleanup 不得盲删（身份核对见 TestCleanupIdentity）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.hub.mu.Lock()
		got := f.hub.conns[makeConnKey("1", "desktop")]
		f.hub.mu.Unlock()
		if got != nil && got.sid == sidB {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("新连接未接管索引槽位（got sid=%v）", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = b // b 保持存活即可
}

// TestSessionSilentReplaceSameSID 同 uid+dev+sid 重连（会话半开被替换）：
// 旧连接不收 session.kicked，直接 close 4000 静默替换。
func TestSessionSilentReplaceSameSID(t *testing.T) {
	f := newChatFixture(t)
	sid := strings.Repeat("c", 32)

	a := f.dial("1:desktop:"+sid, "alice-A")
	a.recvUntil(TypeHello)
	_ = f.dial("1:desktop:"+sid, "alice-B") // 同 sid 顶替

	a.readExpectClose(CloseCodeSessionReplaced)
}

// TestCleanupIdentityNoBlindDelete 盲删防护（§0.8）：被顶替的旧连接跑 cleanup 时，
// conns 槽位与房间成员登记都已是新连接——不得按 uid 盲删。
func TestCleanupIdentityNoBlindDelete(t *testing.T) {
	st := store.NewMem()
	hub, _ := newHubServer(t, st, false)

	old := &client{hub: hub, userID: "1", username: "alice", dev: "desktop", sid: strings.Repeat("a", 32),
		send: make(chan []byte, 8), connID: 1}
	cur := &client{hub: hub, userID: "1", username: "alice", dev: "desktop", sid: strings.Repeat("b", 32),
		send: make(chan []byte, 8), connID: 2}

	hub.mu.Lock()
	key := makeConnKey("1", "desktop")
	hub.conns[key] = cur
	room := &roomState{id: "9", members: map[string]*client{"1": cur}}
	hub.rooms["9"] = room
	cur.setRoomID("9")
	old.setRoomID("9") // 顶替前的残留：旧连接自认为还在房里
	hub.mu.Unlock()

	hub.cleanup(old)

	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.conns[key] != cur {
		t.Fatal("cleanup 盲删了新连接的索引槽位")
	}
	if room.members["1"] != cur {
		t.Fatal("leaveRoom 盲删了新连接的成员登记")
	}
	if cur.roomIDLocked() != "9" {
		t.Fatal("leaveRoom 误清了新连接的 roomID")
	}
	if old.roomIDLocked() != "" {
		t.Fatal("旧连接自身的 roomID 应被清空")
	}
}

// TestHeartbeatClosesZombie 心跳（§0.7）：客户端不读（不回 Pong）→
// ping 超时后 CloseNow，hub 索引随 cleanup 清空。
func TestHeartbeatClosesZombie(t *testing.T) {
	st := store.NewMem()
	hub, hs := newHubServer(t, st, false)
	hub.heartbeatEvery = 20 * time.Millisecond
	hub.pongWait = 50 * time.Millisecond

	conn := dialTo(t, hs, "1") // 故意不 Read：pong 无法被处理
	waitConnsEmpty(t, hub)

	// 服务端已 CloseNow：排空缓冲（hello 等）后必然读到关闭错误。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, _, err := conn.Read(ctx)
		cancel()
		if err != nil {
			return
		}
	}
	t.Fatal("僵尸连接应已被服务端关闭")
}

// ---- T4：Hub.Kick ----

// TestHubKickDevices Kick 导出（§0.11）：dev 空 = 全部设备；指定 dev 只踢该设备。
func TestHubKickDevices(t *testing.T) {
	f := newChatFixture(t)

	// 用户 1：双设备，Kick 全部
	d1 := f.dial("1:desktop:"+strings.Repeat("d", 32), "alice-desk")
	m1 := f.dial("1:mobile:"+strings.Repeat("m", 32), "alice-mob")
	d1.recvUntil(TypeHello)
	m1.recvUntil(TypeHello)
	f.hub.Kick("1", "", "logout")
	for _, c := range []*wsClient{d1, m1} {
		env := c.recvUntil(TypeSessionKicked)
		var kicked SessionKickedData
		if err := env.Decode(&kicked); err != nil {
			t.Fatalf("decode session.kicked: %v", err)
		}
		if kicked.Reason != "logout" {
			t.Fatalf("reason = %q, want logout", kicked.Reason)
		}
		c.readExpectClose(CloseCodeNewLogin)
	}
	waitConnsEmpty(t, f.hub)

	// 用户 2：只踢 mobile，desktop 保持连接
	d2 := f.dial("2:desktop:"+strings.Repeat("e", 32), "bob-desk")
	m2 := f.dial("2:mobile:"+strings.Repeat("f", 32), "bob-mob")
	d2.recvUntil(TypeHello)
	m2.recvUntil(TypeHello)
	f.hub.Kick("2", "mobile", KickReasonLoginElsewhere)
	m2.recvUntil(TypeSessionKicked)
	m2.readExpectClose(CloseCodeNewLogin)

	deadline := time.Now().Add(2 * time.Second)
	for {
		f.hub.mu.Lock()
		_, mobOK := f.hub.conns[makeConnKey("2", "mobile")]
		desk := f.hub.conns[makeConnKey("2", "desktop")]
		f.hub.mu.Unlock()
		if !mobOK && desk != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("设备级 Kick 后索引不符：mobile仍在=%v desktop在=%v", mobOK, desk != nil)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- T4/T5：握手 sid 比对 + 语音单处 ----

// TestWSSessionHandshakeSid WS 握手比对会话 sid（§0.4）：
// 当前 sid → 放行；异 sid / 旧令牌（空 sid）/ 无会话 → 401 + X-LiteTalk-Auth: session_replaced。
func TestWSSessionHandshakeSid(t *testing.T) {
	st := store.NewMem()
	sess := auth.NewSessionIndex(st, nil)
	ctx := context.Background()
	curSid, err := sess.Upsert(ctx, 1, "desktop", "127.0.0.1")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, hs := newHubServerSess(t, st, sess)

	// 当前 sid：握手成功。
	conn := dialTo(t, hs, "1:desktop:"+curSid)
	c := &wsClient{t: t, conn: conn, user: "alice"}
	c.recvUntil(TypeHello)

	// 异 sid：401 + session_replaced 头。
	bad := strings.Repeat("0", 32)
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws?token=" + "1:desktop:" + bad
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("异 sid 握手应失败")
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("握手状态码 = %v, want 401", resp)
	}
	if got := resp.Header.Get("X-LiteTalk-Auth"); got != "session_replaced" {
		t.Fatalf("X-LiteTalk-Auth = %q, want session_replaced", got)
	}

	// 旧令牌（无 sid，testAuthenticator 默认 sid 非当前会话）：同样拒绝。
	url = "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws?token=" + "1"
	if _, resp, err = websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("旧令牌（sid 不符）握手应失败")
	} else if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("握手状态码 = %v, want 401", resp)
	}

	// 会话不存在的用户：拒绝。
	url = "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws?token=" + "2:desktop:" + strings.Repeat("b", 32)
	if _, resp, err = websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("无会话记录握手应失败")
	} else if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("握手状态码 = %v, want 401", resp)
	}
}

// TestVoiceTakenSameAccount 语音单处（§0.10）：desktop 在房，同账号 mobile 进同一语音房
// → desktop 收 session.voice_taken（连接不断），成员登记转移给 mobile。
func TestVoiceTakenSameAccount(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	desk := f.dial("1:desktop:"+strings.Repeat("d", 32), "alice-desk")
	mob := f.dial("1:mobile:"+strings.Repeat("m", 32), "alice-mob")

	desk.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	desk.recvUntil(TypeChannelJoined)

	mob.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	// desktop 先收 voice_taken，且不收 peer_left/joined（已被摘出成员表）。
	env := desk.recvUntil(TypeSessionVoiceTaken)
	if env.Type != TypeSessionVoiceTaken {
		t.Fatalf("期望 session.voice_taken，收到 %q", env.Type)
	}
	mob.recvUntil(TypeChannelJoined)

	f.hub.mu.Lock()
	room := f.hub.rooms[chID]
	mobC := f.hub.conns[makeConnKey("1", "mobile")]
	deskC := f.hub.conns[makeConnKey("1", "desktop")]
	f.hub.mu.Unlock()
	if room == nil || room.members["1"] != mobC {
		t.Fatalf("成员登记未转移给 mobile 连接")
	}
	if deskC == nil || deskC.roomIDLocked() != "" {
		t.Fatal("desktop 连接应已退出语音房")
	}
	// desktop 连接本身不被踢（voice_taken 不关 WS）。
	if deskC.conn == nil {
		t.Fatal("desktop 连接不应被销毁")
	}
}

// ---- T6：chat 类型校验与推送范围 ----

// TestChatTypeValidation chat.send 仅接受 type=text（§0.12）；
// 缺 type 字段按 text 放行（v1.0.x 客户端兼容，P1-3），显式非 text 才拒。
func TestChatTypeValidation(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)

	// 缺 type：默认按 text 放行，不收到 error。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Content: "缺 type"})
	a.recvUntil(TypeChatMessage)

	// 显式非 text：拒绝。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "file", Content: "x"})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeBadMessage {
		t.Fatalf("code = %q, want %q", e.Code, ErrCodeBadMessage)
	}

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "ok"})
	if env := a.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
		t.Fatalf("type=text 应广播，实际 %q", env.Type)
	}
}

// TestChatBroadcastServerWide 广播范围（§0.12）：文字频道消息推送到该服务器
// 全部在线成员（即便不在语音房）；非服务器成员收不到。
func TestChatBroadcastServerWide(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10) // 文本频道；全员不进任何语音房

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	m := f.dial("9", "mallory")
	eve := f.dial("8", "eve") // 非该服务器成员
	for _, c := range []*wsClient{a, b, m, eve} {
		c.recvUntil(TypeHello)
	}

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "全体成员可见"})

	for _, c := range []*wsClient{a, b, m} {
		env := c.recvUntil(TypeChatMessage)
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "全体成员可见" || msg.ChannelID != chID {
			t.Fatalf("消息不符: %+v", msg)
		}
	}

	// eve（非成员）不应收到任何 chat.message。
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, data, err := eve.conn.Read(ctx)
	if err == nil {
		t.Fatalf("非成员不应收到广播，实际 %s", data)
	}
}
