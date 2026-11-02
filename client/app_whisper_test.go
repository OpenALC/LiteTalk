package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/OpenALC/LiteTalk/client/core/voice"
	"github.com/coder/websocket"
)

// #45：悄悄话旁路门与优先发言授予的客户端逻辑。

// TestWhisperGateFlow 按住悄悄话键在无目标时不开麦、有目标时旁路放行；
// 释放恢复 PTT 语义。发送到会话的部分在无会话时为空操作，不 panic。
func TestWhisperGateFlow(t *testing.T) {
	app := &App{hkGate: &hotkey.Gate{}}
	app.hkGate.SetMode(hotkey.ModePTT)

	// 无目标：按键不生效。
	_ = app.SetWhisperTargets(nil)
	app.whisperPress()
	if app.hkGate.Allow(false) {
		t.Fatal("无目标时悄悄话不应开麦")
	}
	app.whisperRelease()

	// 有目标：按住放行，松开恢复 PTT 收门。
	_ = app.SetWhisperTargets([]string{"u1", "u1", ""})
	app.whisperPress()
	if !app.hkGate.Allow(false) {
		t.Fatal("有目标时按住悄悄话应放行上行")
	}
	app.whisperRelease()
	if app.hkGate.Allow(false) {
		t.Fatal("释放悄悄话后应恢复 PTT 收门")
	}

	// 按住期间清空目标：立即关旁路门（回到 PTT 收门）。
	app.whisperPress()
	_ = app.SetWhisperTargets([]string{})
	if app.hkGate.Allow(false) {
		t.Fatal("按住期间清空目标应立即关麦")
	}
	app.whisperRelease()
}

// TestWhisperGateCoexistsWithPTT 先松 PTT 再松悄悄话（或相反）都不能误关麦。
func TestWhisperGateCoexistsWithPTT(t *testing.T) {
	app := &App{hkGate: &hotkey.Gate{}}
	app.hkGate.SetMode(hotkey.ModePTT)
	_ = app.SetWhisperTargets([]string{"u1"})

	app.hkGate.Press()
	app.whisperPress()
	app.hkGate.Release(0) // 松开 PTT：悄悄话仍按住，必须继续放行
	if !app.hkGate.Allow(false) {
		t.Fatal("悄悄话按住时松开 PTT 不得关麦")
	}
	app.whisperRelease()
	if app.hkGate.Allow(false) {
		t.Fatal("两者都释放后应收门")
	}
}

// ---------- #116：协议门控与 ack 时序 ----------

// whisperTestStub 可协商协议版本、记录上行、可推送下行的 WS 桩。
type whisperTestStub struct {
	*httptest.Server

	mu       sync.Mutex
	conn     *websocket.Conn
	received []signal.Envelope
}

func newWhisperTestStub(t *testing.T, version int) *whisperTestStub {
	t.Helper()
	stub := &whisperTestStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		hello := signal.Envelope{
			Type: signal.TypeServerHello, TS: 1,
			Data: json.RawMessage(fmt.Sprintf(`{"version":%d,"session_id":"S1"}`, version)),
		}
		raw, err := json.Marshal(hello)
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
			return
		}
		stub.mu.Lock()
		stub.conn = conn
		stub.mu.Unlock()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env signal.Envelope
			if json.Unmarshal(raw, &env) != nil {
				continue
			}
			stub.mu.Lock()
			stub.received = append(stub.received, env)
			stub.mu.Unlock()
		}
	}))
	t.Cleanup(stub.Server.Close)
	return stub
}

func (s *whisperTestStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *whisperTestStub) push(t *testing.T, env signal.Envelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		t.Fatal("无连接可推送")
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func (s *whisperTestStub) whispers() []signal.VoiceWhisperData {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]signal.VoiceWhisperData, 0, len(s.received))
	for _, env := range s.received {
		if env.Type != signal.TypeVoiceWhisper {
			continue
		}
		var d signal.VoiceWhisperData
		if env.Decode(&d) == nil {
			out = append(out, d)
		}
	}
	return out
}

func (s *whisperTestStub) waitWhisper(t *testing.T) signal.VoiceWhisperData {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.whispers(); len(got) > 0 {
			return got[len(got)-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待 voice.whisper 超时")
	return signal.VoiceWhisperData{}
}

// newWhisperApp 与指定协议版本的服务端建立真实会话（Null 媒体后端，不碰声卡），
// 入房后返回可直接驱动悄悄话状态的 App。协议版本只能经 server.hello 协商，
// 不引入测试专用 setter。
func newWhisperApp(t *testing.T, version int) (*App, *voice.Session, *whisperTestStub) {
	t.Helper()
	stub := newWhisperTestStub(t, version)
	app := &App{hkGate: &hotkey.Gate{}}
	app.hkGate.SetMode(hotkey.ModePTT)
	// 与 connectSessionLocked 相同的接线：voice.whisper_ack 经会话回调驱动 App。
	sess := voice.NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", voice.Events{
		OnWhisperAck: func(seq int64, active bool) { app.onWhisperAck(seq, active) },
	}, nil)
	if err := sess.SetAudioBackend(audio.BackendNull); err != nil {
		t.Fatalf("SetAudioBackend: %v", err)
	}
	sess.Connect(stub.wsURL())
	t.Cleanup(sess.Close)
	deadline := time.Now().Add(5 * time.Second)
	for sess.ProtocolVersion() != int64(version) {
		if time.Now().After(deadline) {
			t.Fatalf("等待协议 v%d 协商超时，当前 v%d", version, sess.ProtocolVersion())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// InRoom 在 channel.join 发送前乐观置位；桩不回 channel.joined 也不影响
	// 悄悄话门控用例（不需要真实媒体/成员表）。
	if err := sess.JoinChannel("room1"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	app.mu.Lock()
	app.sess = sess
	app.mu.Unlock()
	return app, sess, stub
}

func ackEnv(seq int64, active bool) signal.Envelope {
	return signal.Envelope{
		Type: signal.TypeVoiceWhisperAck, TS: 2,
		Data: json.RawMessage(fmt.Sprintf(`{"seq":%d,"active":%v}`, seq, active)),
	}
}

// TestWhisperPressProtocolUnsupported 协议 v2：不开门、不发 voice.whisper，
// 事件明示 supported=false；SetWhisperTargets 直接报错并清空选择（#116）。
func TestWhisperPressProtocolUnsupported(t *testing.T) {
	app, _, stub := newWhisperApp(t, 2)
	if app.WhisperSupported() {
		t.Fatal("协议 v2 不应报告支持悄悄话")
	}
	if err := app.SetWhisperTargets([]string{"u1"}); err == nil {
		t.Fatal("协议 v2 下 SetWhisperTargets 应返回错误")
	}
	app.whisperMu.Lock()
	left := len(app.whisperTargets)
	app.whisperMu.Unlock()
	if left != 0 {
		t.Fatalf("协议 v2 应清空目标，剩余 %d", left)
	}

	// 绕过 UI 直接塞目标：按住仍不得开麦/发信令。
	app.whisperMu.Lock()
	app.whisperTargets = []string{"u1"}
	app.whisperMu.Unlock()
	events := make(chan map[string]any, 4)
	app.emitHook = func(name string, data any) {
		if name != "hotkey:whisper" {
			return
		}
		if payload, ok := data.(map[string]any); ok {
			select {
			case events <- payload:
			default:
			}
		}
	}
	app.whisperPress()
	if app.hkGate.Allow(false) {
		t.Fatal("协议 v2 按住悄悄话不得开麦")
	}
	select {
	case ev := <-events:
		if ev["supported"] != false || ev["active"] != false {
			t.Fatalf("事件应明示不支持，实际 %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 hotkey:whisper 事件")
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(stub.whispers()); got != 0 {
		t.Fatalf("协议 v2 不应下发 voice.whisper，实际 %d 条", got)
	}
	app.whisperRelease()
}

// TestWhisperPressV3Immediate 协议 v3：无 ack 机制，按住立即开麦并下发 seq=0。
func TestWhisperPressV3Immediate(t *testing.T) {
	app, _, stub := newWhisperApp(t, 3)
	if !app.WhisperSupported() {
		t.Fatal("协议 v3 应报告支持悄悄话")
	}
	if err := app.SetWhisperTargets([]string{"u1"}); err != nil {
		t.Fatalf("SetWhisperTargets: %v", err)
	}
	app.whisperPress()
	if !app.hkGate.Allow(false) {
		t.Fatal("协议 v3 按住应立即可麦（无 ack 路径）")
	}
	w := stub.waitWhisper(t)
	if w.Seq != 0 {
		t.Fatalf("v3 兼容路径 seq 应为 0，得到 %d", w.Seq)
	}
	if len(w.Targets) != 1 || w.Targets[0] != "u1" {
		t.Fatalf("targets = %v", w.Targets)
	}
	app.whisperRelease()
	if app.hkGate.Allow(false) {
		t.Fatal("释放后应关麦")
	}
}

// TestWhisperPressV4WaitsAck 协议 v4：ack 前保持关麦，匹配 seq 的确认到达后
// 才开麦；不匹配的过期确认被丢弃。
func TestWhisperPressV4WaitsAck(t *testing.T) {
	app, _, stub := newWhisperApp(t, 4)
	if !app.WhisperSupported() {
		t.Fatal("协议 v4 应报告支持悄悄话")
	}
	if err := app.SetWhisperTargets([]string{"u1"}); err != nil {
		t.Fatalf("SetWhisperTargets: %v", err)
	}
	app.whisperPress()
	if app.hkGate.Allow(false) {
		t.Fatal("ack 到达前不得开麦（白名单未生效）")
	}
	w := stub.waitWhisper(t)
	if w.Seq <= 0 {
		t.Fatalf("v4 下发 seq 应 >0，得到 %d", w.Seq)
	}
	// 过期确认（不匹配 seq）：忽略。
	app.onWhisperAck(w.Seq+1, true)
	if app.hkGate.Allow(false) {
		t.Fatal("不匹配 seq 的 ack 不得开麦")
	}
	// 真实确认：开麦。
	stub.push(t, ackEnv(w.Seq, true))
	deadline := time.Now().Add(2 * time.Second)
	for !app.hkGate.Allow(false) {
		if time.Now().After(deadline) {
			t.Fatal("匹配 ack 后 2s 内未开麦")
		}
		time.Sleep(10 * time.Millisecond)
	}
	app.whisperRelease()
	if app.hkGate.Allow(false) {
		t.Fatal("释放后应关麦")
	}
}

// TestWhisperAckTimeout 300ms 内无确认：保持关麦、发 ack_timeout 事件、保留
// 按住态供用户松手重按重试（#116）。
func TestWhisperAckTimeout(t *testing.T) {
	app, _, _ := newWhisperApp(t, 4)
	events := make(chan map[string]any, 8)
	app.emitHook = func(name string, data any) {
		if name != "hotkey:whisper" {
			return
		}
		if payload, ok := data.(map[string]any); ok {
			select {
			case events <- payload:
			default:
			}
		}
	}
	if err := app.SetWhisperTargets([]string{"u1"}); err != nil {
		t.Fatalf("SetWhisperTargets: %v", err)
	}
	app.whisperPress()
	if app.hkGate.Allow(false) {
		t.Fatal("ack 到达前不得开麦")
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev["error"] == "ack_timeout" {
				if ev["pressed"] != true || ev["active"] != false {
					t.Fatalf("超时事件 = %+v", ev)
				}
				if app.hkGate.Allow(false) {
					t.Fatal("超时后应保持关麦")
				}
				app.whisperMu.Lock()
				held := app.whisperHeld
				app.whisperMu.Unlock()
				if !held {
					t.Fatal("超时应保留按住态供重试")
				}
				app.whisperRelease()
				return
			}
		case <-deadline:
			t.Fatal("未等到 ack_timeout 事件")
		}
	}
}

// TestWhisperAckMatching 仅「按住且 seq 匹配」的确认生效；松开后迟到的确认
// 与不匹配的过期确认都被丢弃（#116）。
func TestWhisperAckMatching(t *testing.T) {
	app := &App{hkGate: &hotkey.Gate{}}
	app.hkGate.SetMode(hotkey.ModePTT)
	app.whisperMu.Lock()
	app.whisperHeld = true
	app.whisperSeq = 5
	app.whisperMu.Unlock()

	app.onWhisperAck(4, true) // 过期
	if app.hkGate.Allow(false) {
		t.Fatal("过期 ack 不得开麦")
	}
	app.onWhisperAck(5, true) // 匹配
	if !app.hkGate.Allow(false) {
		t.Fatal("匹配 ack 应开麦")
	}
	app.whisperRelease()
	app.onWhisperAck(6, true) // 松开后迟到
	if app.hkGate.Allow(false) {
		t.Fatal("松开后的 ack 不得开麦")
	}

	// 取消确认：active=false 只关麦，不因 seq 匹配就当成生效。
	app.whisperMu.Lock()
	app.whisperHeld = true
	app.whisperSeq = 7
	app.whisperActive = true
	app.whisperMu.Unlock()
	app.setWhisperGate(true)
	app.onWhisperAck(7, false)
	if app.hkGate.Allow(false) {
		t.Fatal("active=false 的确认应保持关麦")
	}
	app.whisperRelease()
}

// priorityServer 覆盖接口桩：GET 返回 current；PUT/DELETE 记录请求并更新 current。
type priorityServer struct {
	mu      sync.Mutex
	current string
	puts    []string // "allow|deny"
	deletes int
	fail    int
}

func (p *priorityServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, p.current)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Allow string `json:"allow"`
				Deny  string `json:"deny"`
			}
			_ = json.Unmarshal(body, &req)
			p.puts = append(p.puts, req.Allow+"|"+req.Deny)
			if p.fail != 0 {
				w.WriteHeader(p.fail)
				_, _ = io.WriteString(w, `{"code":"forbidden","detail":"forbidden"}`)
				return
			}
			p.current = fmt.Sprintf(`[{"channel_id":"7","target_type":"member","target_id":"3","allow":%q,"deny":%q,"updated_at":"2026-01-01T00:00:00Z"}]`, req.Allow, req.Deny)
			_, _ = io.WriteString(w, strings.TrimSuffix(strings.TrimPrefix(p.current, "["), "]"))
		case http.MethodDelete:
			p.deletes++
			p.current = "[]"
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// TestSetPrioritySpeaker 保留其他权限位、授予清个人 deny、撤销不写 deny、
// 空覆盖删除、无覆盖撤销为空操作。
func TestSetPrioritySpeaker(t *testing.T) {
	cases := []struct {
		name       string
		current    string
		enabled    bool
		wantPut    string // "" = 不应 PUT
		wantDelete int
	}{
		{
			name:    "授予保留其他位",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"8","deny":"2","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: true, wantPut: "24|2",
		},
		{
			// #117：残留的个人 deny 会让授予在 mergeChannel 里被 deny 盖掉
			// （member 层 (p|allow)&^deny），授予必须同时清这一位。
			name:    "授予清掉个人 deny 位",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"8","deny":"18","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: true, wantPut: "24|2",
		},
		{
			name:    "撤销只清 allow 位",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"16","deny":"2","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false, wantPut: "0|2",
		},
		{
			name:    "撤销后覆盖为空则删除",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false, wantDelete: 1,
		},
		{
			name:    "无个人覆盖授予",
			current: `[{"channel_id":"7","target_type":"role","target_id":"9","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: true, wantPut: "16|0",
		},
		{
			name:    "无个人覆盖撤销是空操作",
			current: `[{"channel_id":"7","target_type":"role","target_id":"9","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false,
		},
		{
			name:    "授予后撤销的往返回到原状态",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"8","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false, wantPut: "8|0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &priorityServer{current: tc.current}
			srv := httptest.NewServer(stub.handler())
			defer srv.Close()
			app := &App{api: newAppAPI(srv.URL)}

			if err := app.SetPrioritySpeaker("7", "3", tc.enabled); err != nil {
				t.Fatalf("SetPrioritySpeaker: %v", err)
			}
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if tc.wantPut == "" {
				if len(stub.puts) != 0 {
					t.Fatalf("不应 PUT，实际 %v", stub.puts)
				}
			} else if len(stub.puts) != 1 || stub.puts[0] != tc.wantPut {
				t.Fatalf("PUT = %v，期望 %q", stub.puts, tc.wantPut)
			}
			if stub.deletes != tc.wantDelete {
				t.Fatalf("DELETE 次数 = %d，期望 %d", stub.deletes, tc.wantDelete)
			}
		})
	}
}

// TestSetPrioritySpeakerDenyRoundTrip 授予 → 撤销往返：授予写 allow+清 deny，
// 撤销重新读回库里的覆盖只清 allow，最终回到授予前的位组合（不残留 deny）。
func TestSetPrioritySpeakerDenyRoundTrip(t *testing.T) {
	// deny 里带 PrioritySpeaker（16）与 ViewChannel（1）：授予后只剩 1。
	stub := &priorityServer{current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"8","deny":"17","updated_at":"2026-01-01T00:00:00Z"}]`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	app := &App{api: newAppAPI(srv.URL)}

	if err := app.SetPrioritySpeaker("7", "3", true); err != nil {
		t.Fatalf("授予: %v", err)
	}
	if err := app.SetPrioritySpeaker("7", "3", false); err != nil {
		t.Fatalf("撤销: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.puts) != 2 || stub.puts[0] != "24|1" || stub.puts[1] != "8|1" {
		t.Fatalf("往返 PUT = %v，期望 [24|1 8|1]", stub.puts)
	}
}

// TestSetPrioritySpeakerRevokeFromRole 撤销个人位改不了 effective 结果时明确
// 报错（#117）：成员仍是优先发言、个人覆盖里却没有该位 → ErrPriorityFromRole，
// 且不写一条注定无效的覆盖。成员标记来自会话成员表。
func TestSetPrioritySpeakerRevokeFromRole(t *testing.T) {
	stub := &priorityServer{current: `[{"channel_id":"7","target_type":"role","target_id":"9","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	app, sess, ws := newWhisperApp(t, 4)
	app.api = newAppAPI(srv.URL)
	ws.push(t, signal.Envelope{
		Type: signal.TypeChannelJoined, TS: 3,
		Data: json.RawMessage(`{"channel_id":"room1","members":[
			{"user_id":"3","nickname":"丙","mute":false,"deafen":false,"speaking":false,"online":true,"priority":true}
		],"self":{"user_id":"1","nickname":"甲","mute":false,"deafen":false,"speaking":false,"online":true}}`),
	})
	waitPeerPriority(t, sess, "3", true)

	err := app.SetPrioritySpeaker("7", "3", false)
	if !errors.Is(err, ErrPriorityFromRole) {
		t.Fatalf("角色授予下撤销应返回 ErrPriorityFromRole，得到 %v", err)
	}
	stub.mu.Lock()
	puts, deletes := append([]string(nil), stub.puts...), stub.deletes
	stub.mu.Unlock()
	if len(puts) != 0 || deletes != 0 {
		t.Fatalf("不应写库，实际 PUT=%v DELETE=%d", puts, deletes)
	}

	// 成员已不是优先发言（例如角色刚被收回）：撤销仍是空操作，不报错。
	ws.push(t, signal.Envelope{
		Type: signal.TypeVoicePriority, TS: 4,
		Data: json.RawMessage(`{"channel_id":"room1","user_id":"3","priority":false}`),
	})
	waitPeerPriority(t, sess, "3", false)
	if err := app.SetPrioritySpeaker("7", "3", false); err != nil {
		t.Fatalf("无个人覆盖且未生效时撤销应为空操作: %v", err)
	}
}

// waitPeerPriority 等会话成员表的 priority 标记落到期望值（事件经 ws 读循环
// 异步写入），避免与 push 竞态。
func waitPeerPriority(t *testing.T, s *voice.Session, userID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range s.Members() {
			if m.UserID == userID && m.Priority == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("成员 %s 的 priority 未在 5s 内变为 %v", userID, want)
}

// TestSetPrioritySpeakerForbidden 403 原样返回，前端提示权限不足。
func TestSetPrioritySpeakerForbidden(t *testing.T) {
	stub := &priorityServer{
		current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"0","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
		fail:    http.StatusForbidden,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	app := &App{api: newAppAPI(srv.URL)}
	if err := app.SetPrioritySpeaker("7", "3", true); err == nil {
		t.Fatal("403 应返回错误")
	}
}
