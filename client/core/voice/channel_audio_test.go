package voice

import (
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
)

// #46 频道音质：码率合并规则与帧长归一（纯函数 + 管线热应用）。

func TestEffectiveBitrateKbps(t *testing.T) {
	cases := []struct {
		user, cap, want int
	}{
		{32, 0, 32},  // 无上限 = 跟随用户
		{32, 48, 32}, // 上限更高不提升用户码率
		{48, 16, 16}, // 上限更低取上限
		{32, 32, 32}, // 相等不变
		{8, 128, 8},  // 用户可低于上限
		{32, -1, 32}, // 非法上限按无限处理（配置层已校验，防御）
	}
	for _, c := range cases {
		if got := effectiveBitrateKbps(c.user, c.cap); got != c.want {
			t.Fatalf("effectiveBitrateKbps(%d,%d) = %d, want %d", c.user, c.cap, got, c.want)
		}
	}
}

func TestNormalizeChannelFrameMS(t *testing.T) {
	cases := map[int]int{0: 20, 10: 10, 20: 20, 15: 20, -1: 20}
	for in, want := range cases {
		if got := normalizeChannelFrameMS(in); got != want {
			t.Fatalf("normalizeChannelFrameMS(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestChannelAudioApply 用户设置与频道上限的合并、帧长热应用与退房恢复。
func TestChannelAudioApply(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.pipe = newNullPipeline(t)
	apply := func(kbps int) {
		t.Helper()
		if err := s.ApplyAudio(audio.RuntimeSettings{
			Opus:            audio.OpusConfig{BitrateKbps: kbps},
			DenoiseStrength: 2,
			Gain:            1,
			PlaybackGain:    1,
		}); err != nil {
			t.Fatalf("ApplyAudio(%d): %v", kbps, err)
		}
	}
	apply(48)

	// 频道上限 16 + 10ms：生效码率取上限，帧长切到 10ms。
	s.setChannelAudio(16, 10)
	if got := s.pipe.FrameMS(); got != 10 {
		t.Fatalf("帧长 = %d, want 10", got)
	}
	s.audioCfgMu.Lock()
	gotUser, gotCap, gotFrame := s.userBitrateKbps, s.chanBitrateKbps, s.chanFrameMS
	s.audioCfgMu.Unlock()
	if gotUser != 48 || gotCap != 16 || gotFrame != 10 {
		t.Fatalf("配置状态 user=%d cap=%d frame=%d", gotUser, gotCap, gotFrame)
	}
	if got := effectiveBitrateKbps(gotUser, gotCap); got != 16 {
		t.Fatalf("生效码率 = %d, want 16", got)
	}

	// 上限取消：恢复用户 48，帧长回 20。
	s.setChannelAudio(0, 0)
	if got := s.pipe.FrameMS(); got != 20 {
		t.Fatalf("取消上限后帧长 = %d, want 20", got)
	}
	s.audioCfgMu.Lock()
	if got := effectiveBitrateKbps(s.userBitrateKbps, s.chanBitrateKbps); got != 48 {
		s.audioCfgMu.Unlock()
		t.Fatalf("取消上限后生效码率 = %d, want 48", got)
	}
	s.audioCfgMu.Unlock()

	// 上限 24 时用户调到 8：取更小的 8；用户调高到 64 仍被 24 限制。
	s.setChannelAudio(24, 20)
	apply(8)
	s.audioCfgMu.Lock()
	if got := effectiveBitrateKbps(s.userBitrateKbps, s.chanBitrateKbps); got != 8 {
		s.audioCfgMu.Unlock()
		t.Fatalf("用户 8 + 上限 24 = %d, want 8", got)
	}
	s.audioCfgMu.Unlock()
	apply(64)
	s.audioCfgMu.Lock()
	if got := effectiveBitrateKbps(s.userBitrateKbps, s.chanBitrateKbps); got != 24 {
		s.audioCfgMu.Unlock()
		t.Fatalf("用户 64 + 上限 24 = %d, want 24", got)
	}
	s.audioCfgMu.Unlock()

	// 退房：上限清除、帧长回 20，用户设置 64 恢复。
	s.resetChannelAudio()
	if got := s.pipe.FrameMS(); got != 20 {
		t.Fatalf("退房后帧长 = %d, want 20", got)
	}
	s.audioCfgMu.Lock()
	defer s.audioCfgMu.Unlock()
	if s.chanBitrateKbps != 0 {
		t.Fatalf("退房后上限应清零: %d", s.chanBitrateKbps)
	}
	if got := effectiveBitrateKbps(s.userBitrateKbps, s.chanBitrateKbps); got != 64 {
		t.Fatalf("退房后生效码率 = %d, want 64", got)
	}
}
