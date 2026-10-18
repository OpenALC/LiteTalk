package signal

import (
	"encoding/json"
	"strconv"
	"testing"
)

// TestVoiceSpeakingFanout 说话翻转下发（#33 裁定 #5）：v2 连接走事件面
// voice.speaking（含本人、进环带 seq），v1/未协商连接退化为 voice.state{speaking}。
func TestVoiceSpeakingFanout(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	// alice 协商到 v2；bob 不发 client.hello（v1/老客户端路径）。
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	a.send(TypeClientHello, ClientHelloData{Min: 2, Max: 2})
	a.recvUntil(TypeServerHello)
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	f.hub.broadcastVoiceSpeaking(chID, "1", true)

	// v2：voice.speaking 走事件面（seq 进环），含本人（自己的声波球同源）。
	env := a.recvUntil(TypeVoiceSpeaking)
	if env.Seq <= 0 {
		t.Fatalf("voice.speaking seq = %d, want > 0（事件面）", env.Seq)
	}
	var sp VoiceSpeakingData
	if err := env.Decode(&sp); err != nil {
		t.Fatalf("decode voice.speaking: %v", err)
	}
	if sp.UserID != "1" || sp.ChannelID != chID || !sp.Speaking {
		t.Fatalf("voice.speaking = %+v, want {1 %s true}", sp, chID)
	}

	// v1：只认 voice.state{speaking}，形状与今天一致（含服务端补的 user_id）。
	env = b.recvUntil(TypeVoiceState)
	var st VoiceStateData
	if err := env.Decode(&st); err != nil {
		t.Fatalf("decode voice.state: %v", err)
	}
	if st.UserID != "1" || st.Speaking == nil || !*st.Speaking {
		t.Fatalf("v1 退化 voice.state = %+v, want {user:1 speaking:true}", st)
	}
}

// TestVoiceStateDropsSpeakingForV2 v2 客户端的 voice.state{speaking} 被服务端忽略
// （#33）：协议 2 起说话判定走 RFC 6464，voice.state 只承载 mute/deafen。
func TestVoiceStateDropsSpeakingForV2(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	a.send(TypeClientHello, ClientHelloData{Min: 2, Max: 2})
	a.recvUntil(TypeServerHello)
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	// v2 客户端上报 speaking（违规但可能发生）+ mute（合法）。
	a.send(TypeVoiceState, VoiceStateData{Speaking: boolPtr(true), Mute: boolPtr(true)})

	env := b.recvUntil(TypeVoiceState)
	var st VoiceStateData
	if err := env.Decode(&st); err != nil {
		t.Fatalf("decode voice.state: %v", err)
	}
	if st.UserID != "1" {
		t.Fatalf("user_id = %q, want 1", st.UserID)
	}
	if st.Speaking != nil {
		t.Fatalf("v2 的 speaking 未被剥离: %+v", st)
	}
	if st.Mute == nil || !*st.Mute {
		t.Fatalf("mute 丢失: %+v", st)
	}
	raw, _ := json.Marshal(st)
	t.Logf("v1 收到的 voice.state: %s", raw)
}
