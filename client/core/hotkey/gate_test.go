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
