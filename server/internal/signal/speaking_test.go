package signal

import (
	"encoding/json"
	"strconv"
	"testing"
)

// TestVoiceSpeakingFanout 说话翻转下发（#33 裁定 #5）：v2 连接即时收
// voice.speaking（含本人，seq=0 不进补发环），v1/未协商连接退化为 voice.state{speaking}。
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

	// v2：voice.speaking 即时下行、seq=0（不进补发环），含本人（声波球同源）。
	env := a.recvUntil(TypeVoiceSpeaking)
	if env.Seq != 0 {
		t.Fatalf("voice.speaking seq = %d, want 0（不进补发环）", env.Seq)
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

	// 说话事件不占补发环：随后的聊天事件从 seq=1 开始，不被挤出窗口。
	// 聊天走文字频道（#35 B3：语音频道拒收文字）。
	b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "after-speaking"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != seqOfChat(1) {
		t.Fatalf("聊天 seq = %d, want %d（说话事件不应占环）", env.Seq, seqOfChat(1))
	}
	// 环内是 chat(1)+channel.unread(2)，无 voice.speaking。chat.message 到达时
	// unread 可能仍在查库（#40），等环到 seq 2 再断言。
	waitRingSeq(t, f, "sa", seqOfChat(1)+1)
	if got := f.hub.evlog.LatestSeq("sa"); got != seqOfChat(1)+1 {
		t.Fatalf("环 latest_seq = %d, want %d（只含聊天与未读事件）", got, seqOfChat(1)+1)
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
