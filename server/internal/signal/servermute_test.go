package signal

import (
	"errors"
	"strconv"
	"testing"

	"github.com/pion/webrtc/v4"
)

// #36 服务端强制闭麦：voice.state{server_muted} 广播、上行伪造忽略、错误口径。

// sfuStubSender 空 SignalSender：只为让 SFU 侧 peer 落位，不走 WS 的 rtc 通路。
type sfuStubSender struct{}

func (sfuStubSender) SendOffer(string)                      {}
func (sfuStubSender) SendCandidate(webrtc.ICECandidateInit) {}
func (sfuStubSender) SendFailed(string, string, string)     {}

// attachSFUPeer 给 uid 建一个真实 SFU peer（pion 上行轨 + 初始 offer）。
// 不完成 ICE——SetServerMuted 只要求 peer 存在，媒体路径由 sfu 包测试覆盖。
func attachSFUPeer(t *testing.T, f *chatFixture, userID string) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "litetalk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	chID := strconv.FormatInt(f.vch.ID, 10)
	if _, err := f.hub.sfu.HandleOffer(chID, userID, userID, offer.SDP, sfuStubSender{}); err != nil {
		t.Fatalf("HandleOffer(%s): %v", userID, err)
	}
}

// TestSetServerMutedBroadcast 闭麦/解禁都向房间全体广播 voice.state{server_muted}，
// 目标本人也收（用于状态显示）；目标不在房 / 房不存在 → ErrNotInChannel。
func TestSetServerMutedBroadcast(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)
	attachSFUPeer(t, f, "1")
	attachSFUPeer(t, f, "2")

	if err := f.hub.SetServerMuted(vchID, "2", true); err != nil {
		t.Fatalf("SetServerMuted(true): %v", err)
	}
	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeVoiceState)
		var d VoiceStateData
		if err := env.Decode(&d); err != nil {
			t.Fatalf("decode voice.state: %v", err)
		}
		if d.UserID != "2" || d.ServerMuted == nil || !*d.ServerMuted {
			t.Fatalf("闭麦广播应 server_muted=true: %+v", d)
		}
	}

	if err := f.hub.SetServerMuted(vchID, "2", false); err != nil {
		t.Fatalf("SetServerMuted(false): %v", err)
	}
	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeVoiceState)
		var d VoiceStateData
		if err := env.Decode(&d); err != nil {
			t.Fatalf("decode voice.state: %v", err)
		}
		if d.ServerMuted == nil || *d.ServerMuted {
			t.Fatalf("解禁广播应 server_muted=false: %+v", d)
		}
	}

	// 目标不在房（eve 从未 join）/ 房不存在。
	if err := f.hub.SetServerMuted(vchID, "8", true); !errors.Is(err, ErrNotInChannel) {
		t.Fatalf("目标不在房应 ErrNotInChannel，实际 %v", err)
	}
	if err := f.hub.SetServerMuted("4242424242", "2", true); !errors.Is(err, ErrNotInChannel) {
		t.Fatalf("房不存在应 ErrNotInChannel，实际 %v", err)
	}
}

// TestVoiceStateUplinkIgnoresServerMuted C2S 伪造 server_muted 被服务端忽略（#36），
// 同一条里的 mute 等常规字段照常转发。
func TestVoiceStateUplinkIgnoresServerMuted(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	tr := true
	a.send(TypeVoiceState, VoiceStateData{UserID: "1", ServerMuted: &tr, Mute: &tr})
	env := b.recvUntil(TypeVoiceState)
	var d VoiceStateData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode voice.state: %v", err)
	}
	if d.ServerMuted != nil {
		t.Fatalf("上行 server_muted 不得被转发: %+v", d)
	}
	if d.Mute == nil || !*d.Mute {
		t.Fatalf("常规 mute 字段应照常转发: %+v", d)
	}
}
