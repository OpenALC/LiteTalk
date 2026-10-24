// 续传联调测试（#32/#81）：真实 Hub + 真实客户端栈（client/core/signal 的
// WS 客户端 + client/core/gateway.Tracker），走完整握手/协商/续传/失效路径。
//
// 两侧各自的单测都用桩，曾把本组 P0 全部漏掉（客户端桩自己塞 session_id）。
// 因此这里不允许手写 session_id / seq：session_id 来自 server.hello，
// resume 决策与 seq 投递都由真实 Tracker 产生。
package signal

import (
	"context"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/gateway"
	clientsignal "github.com/OpenALC/LiteTalk/client/core/signal"
)

// gwClient 一条真实客户端信令连接（自动协商 + Gate + Tracker）。
type gwClient struct {
	t      *testing.T
	cli    *clientsignal.Client
	cancel context.CancelFunc
	done   chan struct{}

	resumes chan clientsignal.GatewayResumeData // Tracker 决定的 resume 载荷
	chats   chan clientsignal.Envelope          // Gate 放行的 chat.message（含 seq）
}

// startGatewayClient 用真实客户端栈连到 fixture 的 Hub。tr 在跨连接（重连/
// 服务端重启）时复用，与 desktop 的 Session 生命周期一致。
func startGatewayClient(t *testing.T, f *chatFixture, token string, tr *gateway.Tracker) *gwClient {
	t.Helper()
	g := &gwClient{
		t:       t,
		done:    make(chan struct{}),
		resumes: make(chan clientsignal.GatewayResumeData, 8),
		chats:   make(chan clientsignal.Envelope, 32),
	}
	wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"
	cli := clientsignal.NewClient(clientsignal.Config{
		URL:          wsURL,
		TokenFunc:    func(context.Context) (string, error) { return token, nil },
		Gate:         tr.Gate,
		Logger:       log.New(io.Discard, "", 0),
		ReconnectMin: 50 * time.Millisecond,
		ReconnectMax: 200 * time.Millisecond,
	})
	// 与 client/core/voice/session.go 的接线一致：server.hello → Tracker 决策 →
	// 需要时发 gateway.resume。这里不手工构造 resume 载荷。
	cli.On(clientsignal.TypeServerHello, func(env clientsignal.Envelope) {
		var d clientsignal.ServerHelloData
		if env.Decode(&d) != nil || d.Version < 1 {
			return
		}
		if resume, ok := tr.OnServerHello(d.SessionID, d.Version); ok {
			select {
			case g.resumes <- resume:
			default:
			}
			_ = cli.Send(clientsignal.TypeGatewayResume, resume)
		}
	})
	cli.On(clientsignal.TypeChatMessage, func(env clientsignal.Envelope) {
		select {
		case g.chats <- env:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	g.cli = cli
	g.cancel = cancel
	go func() {
		defer close(g.done)
		_ = cli.Run(ctx)
	}()
	t.Cleanup(g.stop)
	return g
}

// stop 断开并等待 Run 退出（幂等）。
func (g *gwClient) stop() {
	g.cancel()
	g.cli.Close()
	<-g.done
}

// waitResume 等 Tracker 产生一次 resume 决策。
func (g *gwClient) waitResume() clientsignal.GatewayResumeData {
	g.t.Helper()
	select {
	case r := <-g.resumes:
		return r
	case <-time.After(5 * time.Second):
		g.t.Fatal("5s 内未产生 gateway.resume")
		return clientsignal.GatewayResumeData{}
	}
}

// waitChat 等 Gate 放行一条 chat.message。
func (g *gwClient) waitChat() clientsignal.Envelope {
	g.t.Helper()
	select {
	case env := <-g.chats:
		return env
	case <-time.After(5 * time.Second):
		g.t.Fatal("5s 内未收到 chat.message")
		return clientsignal.Envelope{}
	}
}

// bobSend 让对端（bob，事件生产方）发一条消息并确认服务端已进环
// （bob 自己的回显带 seq，收到即代表广播路径完成）。
func bobSend(t *testing.T, b *wsClient, chID, content string) {
	t.Helper()
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: content})
	if env := b.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
		t.Fatalf("发送者未收到回显: %s", env.Type)
	}
}

// expectChat 断言 Gate 按序放行的下一条消息（seq + 内容）。
func (g *gwClient) expectChat(wantSeq int64, wantContent string) {
	g.t.Helper()
	env := g.waitChat()
	if env.Seq != wantSeq {
		g.t.Fatalf("chat seq = %d, want %d", env.Seq, wantSeq)
	}
	var m clientsignal.ChatMessageData
	if err := env.Decode(&m); err != nil {
		g.t.Fatalf("decode chat.message: %v", err)
	}
	if m.Content != wantContent {
		g.t.Fatalf("chat 内容 = %q, want %q", m.Content, wantContent)
	}
}

// waitTrackerSeq 等 Gate 把收件人自己的 channel.unread 也投递完。chat.message
// 与 channel.unread 是两次下行，expectChat 返回时后者可能还在路上（#40）。
func waitTrackerSeq(t *testing.T, tr *gateway.Tracker, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for tr.LastSeq() < want {
		if time.Now().After(deadline) {
			t.Fatalf("Tracker 基准未到 %d（当前 %d）", want, tr.LastSeq())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := tr.LastSeq(); got != want {
		t.Fatalf("Tracker 基准 = %d, want %d", got, want)
	}
}

// TestGatewayIntegrationResumeAfterDisconnect 场景 1：在线收 1..4（每条 chat 后
// 跟一条 channel.unread）→ 断线期间产生 5..8 → 同 sid 重连 → server.hello 带
// session_id → Tracker resume{last_seq:4} → 补发 5..8 按序投递 → 实时事件 9 续流。
func TestGatewayIntegrationResumeAfterDisconnect(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	b := f.dial("2:desktop:sb", "bob")

	tr := gateway.New()
	a1 := startGatewayClient(t, f, "1:desktop:sa", tr)
	// 首次协商就是 resume{0}（#81：复用旧 sid 时环里可能已有事件）。
	if r := a1.waitResume(); r.SessionID != "sa" || r.LastSeq != 0 {
		t.Fatalf("首次 resume = %+v, want {sa 0}", r)
	}
	bobSend(t, b, chID, "m1")
	a1.expectChat(seqOfChat(1), "m1")
	bobSend(t, b, chID, "m2")
	a1.expectChat(seqOfChat(2), "m2")
	waitTrackerSeq(t, tr, seqOfChat(2)+1)

	a1.stop()
	waitOffline(t, f, "1")

	// 断线期间的事件进环（会话窗口内），每次发送各占 chat + unread 两个 seq。
	bobSend(t, b, chID, "m3")
	bobSend(t, b, chID, "m4")

	// 同 sid 重连：server.hello.session_id 命中 Tracker 记录的会话。
	a2 := startGatewayClient(t, f, "1:desktop:sa", tr)
	if r := a2.waitResume(); r.SessionID != "sa" || r.LastSeq != seqOfChat(2)+1 {
		t.Fatalf("重连 resume = %+v, want {sa %d}", r, seqOfChat(2)+1)
	}
	a2.expectChat(seqOfChat(3), "m3")
	a2.expectChat(seqOfChat(4), "m4")
	waitTrackerSeq(t, tr, seqOfChat(4)+1)

	// 补发完毕后实时事件继续流式。
	bobSend(t, b, chID, "m5")
	a2.expectChat(seqOfChat(5), "m5")
}

// TestGatewayIntegrationServerRestart 场景 2：服务端重启（新 Hub/新环），客户端
// 拿旧 sid + 旧基准 resume → invalid_session{latest_seq:0} → Tracker 重置基准 →
// 新事件从 1 继续投递（不会永久卡在旧 seq 上）。
func TestGatewayIntegrationServerRestart(t *testing.T) {
	f1 := newChatFixture(t)
	ch1 := strconv.FormatInt(f1.ch.ID, 10)
	b1 := f1.dial("2:desktop:sb", "bob")

	tr := gateway.New()
	a1 := startGatewayClient(t, f1, "1:desktop:sa", tr)
	a1.waitResume()
	bobSend(t, b1, ch1, "old1")
	a1.expectChat(seqOfChat(1), "old1")
	bobSend(t, b1, ch1, "old2")
	a1.expectChat(seqOfChat(2), "old2")
	waitTrackerSeq(t, tr, seqOfChat(2)+1)
	a1.stop()
	waitOffline(t, f1, "1")

	// 新进程：新 Hub、新事件环（同 sid 的环从未存在过）。fixture 的 store 与
	// 鉴权也一并重建，token 里的 sid 不变——正是「服务端重启后客户端重连」。
	f2 := newChatFixture(t)
	ch2 := strconv.FormatInt(f2.ch.ID, 10)
	b2 := f2.dial("2:desktop:sb", "bob")

	resync := make(chan string, 4)
	tr.OnResync(func(reason string) {
		select {
		case resync <- reason:
		default:
		}
	})

	a2 := startGatewayClient(t, f2, "1:desktop:sa", tr)
	if r := a2.waitResume(); r.SessionID != "sa" || r.LastSeq != seqOfChat(2)+1 {
		t.Fatalf("重启后 resume = %+v, want {sa %d}（客户端仍拿旧基准）", r, seqOfChat(2)+1)
	}
	select {
	case reason := <-resync:
		if reason != "invalid_session" {
			t.Fatalf("resync reason = %q, want invalid_session", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invalid_session 未触发全量补拉")
	}
	if got := tr.LastSeq(); got != 0 {
		t.Fatalf("invalid_session{latest_seq:0} 后基准 = %d, want 0", got)
	}

	// 新环从 1 重计：Tracker 已对齐基准，事件必须能投递。
	bobSend(t, b2, ch2, "new1")
	a2.expectChat(seqOfChat(1), "new1")
	waitTrackerSeq(t, tr, seqOfChat(1)+1)
	bobSend(t, b2, ch2, "new2")
	a2.expectChat(seqOfChat(2), "new2")
}

// TestGatewayIntegrationFreshTrackerReplaysRing 场景 3：客户端 Tracker 全新
// （lastSeq=0）但服务端环里已有 seq>1 的事件（复用旧 sid 的首次协商）。
// 首次 server.hello 也必须发 resume{0}，否则这些事件永久卡住。
func TestGatewayIntegrationFreshTrackerReplaysRing(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	b := f.dial("2:desktop:sb", "bob")

	// 先用同 sid 建环并断开（模拟「旧 sid 的首个连接」）。
	warm := startGatewayClient(t, f, "1:desktop:sa", gateway.New())
	warm.waitResume()
	warm.stop()
	waitOffline(t, f, "1")

	// 离线期间事件写进已存在的环。
	bobSend(t, b, chID, "r1")
	bobSend(t, b, chID, "r2")
	bobSend(t, b, chID, "r3")

	// 全新 Tracker（lastSeq=0）：首次协商发 resume{0}，从 1 开始补齐。
	tr := gateway.New()
	a := startGatewayClient(t, f, "1:desktop:sa", tr)
	if r := a.waitResume(); r.SessionID != "sa" || r.LastSeq != 0 {
		t.Fatalf("首次 resume = %+v, want {sa 0}", r)
	}
	a.expectChat(seqOfChat(1), "r1")
	a.expectChat(seqOfChat(2), "r2")
	a.expectChat(seqOfChat(3), "r3")
	waitTrackerSeq(t, tr, seqOfChat(3)+1)
}
