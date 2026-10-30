package signal

import (
	"encoding/json"
	"io"
	"log"
	"strconv"
	"testing"
	"time"
)

// #116：voice.whisper 携带 seq（协议 v4）时，服务端在白名单真正生效后回
// voice.whisper_ack；旧客户端（seq=0）与协议 <4 连接保持 v3 行为，不回 ack。

// TestSendWhisperAckGating ack 的发送条件矩阵：只有 seq>0 且协议 ≥4 才下发；
// 信封 seq 恒为 0（不进补发环）。
func TestSendWhisperAckGating(t *testing.T) {
	h := &Hub{log: log.New(io.Discard, "", 0)}
	cases := []struct {
		name   string
		proto  int64
		seq    int64
		active bool
		want   bool
	}{
		{"v4 开启", 4, 1, true, true},
		{"v4 取消也回 ack", 4, 2, false, true},
		{"v3 不回（旧客户端路径）", 3, 1, true, false},
		{"v2 不回", 2, 1, true, false},
		{"未协商不回", 0, 1, true, false},
		{"seq=0 不回（旧客户端不带序号）", 4, 0, true, false},
		{"seq<0 不回", 4, -3, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &client{userID: "1", send: make(chan []byte, 4)}
			c.protoVer.Store(tc.proto)
			h.sendWhisperAck(c, tc.seq, tc.active)
			if !tc.want {
				if len(c.send) != 0 {
					t.Fatalf("不应下发 ack，send 队列有 %d 条", len(c.send))
				}
				return
			}
			if len(c.send) != 1 {
				t.Fatalf("应下发 1 条 ack，实际 %d", len(c.send))
			}
			var env Envelope
			if err := json.Unmarshal(<-c.send, &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env.Type != TypeVoiceWhisperAck {
				t.Fatalf("type = %q，want %q", env.Type, TypeVoiceWhisperAck)
			}
			if env.Seq != 0 {
				t.Fatalf("ack 不进补发环，信封 seq 应为 0，实际 %d", env.Seq)
			}
			var d VoiceWhisperAckData
			if err := env.Decode(&d); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if d.Seq != tc.seq || d.Active != tc.active {
				t.Fatalf("载荷 = %+v，want seq=%d active=%v", d, tc.seq, tc.active)
			}
		})
	}
}

// TestWhisperAckAfterPeerCreation voice.whisper 先于 rtc.offer：消息被处理
// （hub 快照已更新）但 peer 尚不存在时不得立即 ack；建 peer 补应用后才 ack。
func TestWhisperAckAfterPeerCreation(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	if v := negotiate(t, a, 4); v != 4 {
		t.Fatalf("alice 协商版本 = %d，期望 4", v)
	}
	b := f.dial("2", "bob")
	negotiate(t, b, 4)
	joinAndCapture(t, a, vchID)
	joinAndCapture(t, b, vchID)

	// 1) 任何 peer 之前下发：hub 快照可见，但不得回 ack（白名单未生效）。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}, Seq: 7})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")
	f.hub.sendJSON(f.hub.roomMember(vchID, "1"), "test.marker", nil)
	recvUntilNo(t, a, "test.marker", TypeVoiceWhisperAck)

	// 2) rtc.offer 建立 peer → 补应用白名单并发 ack；seq 原样回传。
	ap := newRTCProbe(t, a)
	ap.waitConnected(t)
	ack := ap.waitAck(t, 15*time.Second)
	if ack.Seq != 7 || !ack.Active {
		t.Fatalf("补应用后的 ack = %+v，want seq=7 active=true", ack)
	}
}

// TestWhisperAckActivateCancel peer 已存在：每次 voice.whisper 成功应用后
// 立即 ack；seq=0（旧客户端语义）不回 ack。
func TestWhisperAckActivateCancel(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	negotiate(t, a, 4)
	b := f.dial("2", "bob")
	negotiate(t, b, 4)
	joinAndCapture(t, a, vchID)
	joinAndCapture(t, b, vchID)

	ap := newRTCProbe(t, a)
	ap.waitConnected(t)

	// 开启：targets 非空 → active=true。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}, Seq: 3})
	if ack := ap.waitAck(t, 10*time.Second); ack.Seq != 3 || !ack.Active {
		t.Fatalf("开启 ack = %+v，want seq=3 active=true", ack)
	}

	// 取消：targets 空 → active=false。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}, Seq: 4})
	if ack := ap.waitAck(t, 10*time.Second); ack.Seq != 4 || ack.Active {
		t.Fatalf("取消 ack = %+v，want seq=4 active=false", ack)
	}

	// seq=0 不回 ack：后一条 seq=5 的 ack 是顺序屏障——若前一条也回了 ack，
	// waitAck 会先拿到 seq=0 而不是 5。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}, Seq: 5})
	if ack := ap.waitAck(t, 10*time.Second); ack.Seq != 5 || ack.Active {
		t.Fatalf("seq=0 不应回 ack，实际先收到 %+v", ack)
	}
}
