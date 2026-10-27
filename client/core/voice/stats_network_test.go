package voice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
)

func TestLossPercent(t *testing.T) {
	cases := []struct {
		name     string
		received uint32
		lost     int32
		want     float64
	}{
		{"no packets", 0, 0, 0},
		{"no loss", 100, 0, 0},
		{"5 percent", 95, 5, 5},
		{"negative lost clamps to zero", 100, -3, 0},
		{"all lost", 0, 7, 100},
	}
	for _, c := range cases {
		if got := lossPercent(c.received, c.lost); got != c.want {
			t.Errorf("%s: lossPercent(%d, %d) = %v, want %v", c.name, c.received, c.lost, got, c.want)
		}
	}
}

func TestPeerLinkDiag(t *testing.T) {
	ls := audio.RemoteLinkStats{
		SSRC: 42,
		Jitter: rtc.JitterStats{
			JitterMs:      8,
			TargetPackets: 4,
			Buffered:      3,
			Lost:          2,
		},
		PLCFrames: 1,
	}
	inbound := inboundBySSRC([]rtc.InboundStreamStats{{
		SSRC:            42,
		PacketsReceived: 90,
		PacketsLost:     10,
		Jitter:          6_500_000, // 6.5ms
	}})
	d := peerLinkDiag("u1", ls, 20, inbound)

	if d.PeerID != "u1" || d.JitterMs != 8 || d.JitterLost != 2 || d.PLCFrames != 1 {
		t.Fatalf("抖动缓冲字段未透传: %+v", d)
	}
	if d.BufferMS != 60 || d.TargetMS != 80 {
		t.Fatalf("缓冲深度换算错误: buffer=%d target=%d", d.BufferMS, d.TargetMS)
	}
	if d.LossPct != 10 {
		t.Fatalf("LossPct = %v, want 10", d.LossPct)
	}
	if d.RTPJitterMS != 6.5 {
		t.Fatalf("RTPJitterMS = %v, want 6.5", d.RTPJitterMS)
	}
	if d.RTPReceived != 90 || d.RTPLost != 10 {
		t.Fatalf("入站计数错误: %+v", d)
	}

	// 入站流尚未出现：只输出缓冲侧字段，不产生假的 0% 丢包。
	empty := peerLinkDiag("u2", audio.RemoteLinkStats{SSRC: 7, Jitter: rtc.JitterStats{TargetPackets: 3, Buffered: 1}}, 10, nil)
	if empty.BufferMS != 10 || empty.TargetMS != 30 {
		t.Fatalf("10ms 帧长缓冲换算错误: %+v", empty)
	}
	if empty.LossPct != 0 || empty.RTPJitterMS != 0 || empty.RTPReceived != 0 {
		t.Fatalf("无入站流时不应有丢包/抖动读数: %+v", empty)
	}

	// frameMS 零值回落 20ms，不能算出 0 缓冲。
	fallback := peerLinkDiag("u3", audio.RemoteLinkStats{SSRC: 9, Jitter: rtc.JitterStats{Buffered: 2}}, 0, nil)
	if fallback.BufferMS != 40 {
		t.Fatalf("frameMS 零值应回落 20ms，得到 %d", fallback.BufferMS)
	}
}

// TestStatsOmitsZeroNetworkFields 未入房/无下行流时新增字段保持省略，旧前端
// 看到的 JSON 形状不变（#49 B 向后兼容）。
func TestStatsOmitsZeroNetworkFields(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	raw := s.Stats()
	for _, key := range []string{"loss_pct", "rtp_jitter_ms", "jitter_buffer_ms", "jitter_target_ms", "rtp_received", "rtp_lost"} {
		if strings.Contains(raw, `"`+key+`"`) {
			t.Fatalf("空统计不应输出 %s: %s", key, raw)
		}
	}
	var vs VoiceStats
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		t.Fatalf("Stats JSON: %v", err)
	}
	if vs.LatencyMS != 0 {
		t.Fatalf("无 PC 时延迟应为 0, 得到 %d", vs.LatencyMS)
	}
}
