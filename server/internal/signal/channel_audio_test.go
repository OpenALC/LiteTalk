package signal

import (
	"context"
	"strconv"
	"testing"
)

// #46：频道音质配置经 channel.joined 下发、channel.audio 热更新。

// setVchAudio 设置语音房码率上限/帧长（全行写语义：先读后写）。
func setVchAudio(t *testing.T, f *chatFixture, bitrateKbps, frameMS int) {
	t.Helper()
	ch := loadVch(t, f)
	ch.AudioBitrateKbps = bitrateKbps
	ch.AudioFrameMS = frameMS
	if err := f.st.UpdateChannel(context.Background(), ch); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
}

// TestChannelJoinedAudioConfig 入房快照带权威音质配置；未配置的频道为 0/0。
func TestChannelJoinedAudioConfig(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	// 默认（0/0）也显式下发，客户端不必区分「字段缺省」与「跟随设置」。
	a := f.dial("1", "alice")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	var joined ChannelJoinedData
	if err := a.recvUntil(TypeChannelJoined).Decode(&joined); err != nil {
		t.Fatalf("decode channel.joined: %v", err)
	}
	if joined.AudioBitrateKbps == nil || *joined.AudioBitrateKbps != 0 ||
		joined.AudioFrameMs == nil || *joined.AudioFrameMs != 0 {
		t.Fatalf("默认音质应显式下发 0/0: %+v", joined)
	}

	// 改配置后重新入房（先离房）拿到新值。
	setVchAudio(t, f, 24, 10)
	a.send(TypeChannelLeave, ChannelLeaveData{})
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	if err := a.recvUntil(TypeChannelJoined).Decode(&joined); err != nil {
		t.Fatalf("decode channel.joined（更新后）: %v", err)
	}
	if joined.AudioBitrateKbps == nil || *joined.AudioBitrateKbps != 24 ||
		joined.AudioFrameMs == nil || *joined.AudioFrameMs != 10 {
		t.Fatalf("joined 音质配置不符: %+v", joined)
	}
}

// TestBroadcastChannelAudio channel.audio 只投递给指定收件人，载荷原样。
func TestBroadcastChannelAudio(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1", "alice")
	m := f.dial("9", "mallory")

	f.hub.BroadcastChannelAudio([]string{"1"}, "77", 0, 10)
	var got ChannelAudioData
	if err := a.recvUntil(TypeChannelAudio).Decode(&got); err != nil {
		t.Fatalf("alice decode: %v", err)
	}
	if got.ChannelID != "77" || got.AudioBitrateKbps != 0 || got.AudioFrameMs != 10 {
		t.Fatalf("channel.audio 载荷不符: %+v", got)
	}

	// mallory 不在收件人里：不得收到 channel.audio（用后续事件确认顺序）。
	f.hub.BroadcastMessageDeleted([]string{"9"}, "77", "88")
	if env := m.recvUntil(TypeMessageDeleted); env.Type != TypeMessageDeleted {
		t.Fatalf("mallory 不应收到 channel.audio，实际先收到 %q", env.Type)
	}
}
