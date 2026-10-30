package hotkey

import (
	"sync"
	"testing"
	"time"
)

func TestGateZeroValueBlocks(t *testing.T) {
	var g Gate // 零值 = ModePTT 且未按下
	if g.Allow(false) {
		t.Fatal("零值 Gate 应拦住所有上行")
	}
	if g.Allow(true) {
		t.Fatal("PTT 模式下 speaking 不参与判定，仍应拦住")
	}
}

func TestGateModeOpenAlwaysAllows(t *testing.T) {
	var g Gate
	g.SetMode(ModeOpen)
	for _, speaking := range []bool{false, true} {
		if !g.Allow(speaking) {
			t.Errorf("常开模式恒放行，speaking=%v 时应为 true", speaking)
		}
	}
	g.Press()
	g.Release(time.Second)
	if !g.Allow(false) {
		t.Error("常开模式松手后仍应放行")
	}
}

func TestGateModeVoiceUsesSpeaking(t *testing.T) {
	var g Gate
	g.SetMode(ModeVoice)
	if g.Allow(false) {
		t.Error("语音激活模式下静音帧应拦住")
	}
	if !g.Allow(true) {
		t.Error("语音激活模式下语音帧应放行")
	}
	// 切到常开后不再看 speaking
	g.SetMode(ModeOpen)
	if !g.Allow(false) {
		t.Error("常开模式下静音帧也应放行")
	}
	// 切回语音激活，判定重新交给 VAD
	g.SetMode(ModeVoice)
	if g.Allow(false) {
		t.Error("切回语音激活后静音帧应拦住")
	}
	if !g.Allow(true) {
		t.Error("切回语音激活后语音帧应放行")
	}
}

func TestGatePressRelease(t *testing.T) {
	var g Gate
	g.SetMode(ModePTT)
	if g.Allow(false) {
		t.Fatal("未按下时应拦住")
	}
	g.Press()
	if !g.Allow(false) {
		t.Fatal("按下后应放行")
	}
	// Release(0)：不补尾音，立刻收门
	g.Release(0)
	if g.Allow(false) {
		t.Fatal("Release(0) 后不应放行")
	}
	// Release(250ms)：尾包窗内仍放行
	g.Press()
	g.Release(250 * time.Millisecond)
	if !g.Allow(false) {
		t.Fatal("尾包窗内应放行")
	}
	time.Sleep(300 * time.Millisecond)
	if g.Allow(false) {
		t.Fatal("尾包窗超时后应拦住")
	}
}

func TestGateReleaseNegativeTailClosesGate(t *testing.T) {
	var g Gate
	g.SetMode(ModePTT)
	g.Press()
	g.Release(-1)
	if g.Allow(false) {
		t.Fatal("负尾包应视为不补尾音")
	}
}

func TestGateTailWindowClearsPreviousTail(t *testing.T) {
	var g Gate
	g.SetMode(ModePTT)
	g.Press()
	g.Release(time.Minute) // 先开一个长尾包窗
	g.Press()
	g.Release(0) // 再立刻收掉
	if g.Allow(false) {
		t.Fatal("Release(0) 应清掉已开启的尾包窗")
	}
}

func TestGateSetModeSwitches(t *testing.T) {
	var g Gate
	g.SetMode(ModePTT)
	g.Press()
	g.SetMode(ModeOpen)
	if !g.Allow(false) {
		t.Fatal("切到常开后应放行")
	}
	g.SetMode(ModePTT)
	if !g.Allow(false) {
		t.Fatal("切回 PTT 且仍按住时应放行")
	}
	g.Release(0)
	if g.Allow(false) {
		t.Fatal("切回 PTT 且松手后应拦住")
	}
	// 越界模式按 PTT 收敛
	g.SetMode(Mode(99))
	if g.Allow(false) {
		t.Fatal("越界模式应按 PTT 处理")
	}
}

// TestGateEffectiveSendModeIntegration 锁住 #93：出厂配置下发送门必须放行。
// 热键未启用、PTT 未绑定时若仍按 PTT 收门，所有上行帧会被丢弃。
func TestGateEffectiveSendModeIntegration(t *testing.T) {
	var g Gate
	g.SetMode(EffectiveSendMode(DefaultSettings()))
	if !g.Allow(false) {
		t.Fatal("出厂配置下发送门必须放行，否则上行音频全丢")
	}

	// 启用并绑定后恢复 PTT：按下放行、松开拦截。
	g.SetMode(EffectiveSendMode(Settings{Enabled: true, Mode: ModePTT, PTT: "ctrl+m"}))
	if g.Allow(false) {
		t.Fatal("启用 PTT 且未按下时应拦住")
	}
	g.Press()
	if !g.Allow(false) {
		t.Fatal("启用 PTT 且按下后应放行")
	}
	g.Release(0)
	if g.Allow(false) {
		t.Fatal("启用 PTT 松开后应拦住")
	}

	// 语音激活 / 常开语义不变。
	g.SetMode(EffectiveSendMode(Settings{Mode: ModeVoice}))
	if g.Allow(false) || !g.Allow(true) {
		t.Fatal("语音激活模式应只放行 VAD 判定为说话的帧")
	}
	g.SetMode(EffectiveSendMode(Settings{Mode: ModeOpen}))
	if !g.Allow(false) {
		t.Fatal("常开模式应恒放行")
	}
}

// TestGateForceWhisper 悄悄话旁路（#45）：Force 期间无视模式放行，解除后
// 回到 PTT 状态；与 Press/Release 互不覆盖（先松 PTT 再松悄悄话不误关麦）。
func TestGateForceWhisper(t *testing.T) {
	var g Gate
	g.SetMode(ModePTT)
	if g.Allow(false) {
		t.Fatal("PTT 未按下时应拦住")
	}
	g.Force(true)
	if !g.Allow(false) {
		t.Fatal("悄悄话按住时即使 PTT 未按也应放行")
	}

	// PTT 按下 → PTT 松开（开尾包窗）→ 悄悄话仍按住：必须继续放行。
	g.Press()
	g.Release(0)
	if !g.Allow(false) {
		t.Fatal("悄悄话仍按住时松开 PTT 不得关麦")
	}
	g.Force(false)
	if g.Allow(false) {
		t.Fatal("悄悄话释放且 PTT 已松开后应恢复拦帧")
	}

	// 反序：悄悄话先松、PTT 仍按住 → 继续放行。
	g.Press()
	g.Force(true)
	g.Force(false)
	if !g.Allow(false) {
		t.Fatal("PTT 仍按住时释放悄悄话不得关麦")
	}
	g.Release(0)
	if g.Allow(false) {
		t.Fatal("两者都释放后应拦帧")
	}
}

func TestGateConcurrentUse(t *testing.T) {
	var g Gate
	g.SetMode(ModePTT)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				switch (i + j) % 4 {
				case 0:
					g.Press()
				case 1:
					g.Release(time.Millisecond)
				case 2:
					g.SetMode(Mode((i + j) % 3))
				case 3:
					_ = g.Allow(j%2 == 0)
				}
			}
		}(i)
	}
	wg.Wait()
	// 收尾：回到 PTT 且松手，必须收门
	g.SetMode(ModePTT)
	g.Release(0)
	if g.Allow(false) {
		t.Fatal("并发收尾后应拦住")
	}
}
