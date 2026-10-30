package audio

import (
	"testing"
	"time"
)

// newTestMixer 构造输出到本地环的混音器，返回环便于断言。
func newTestMixer() (*Mixer, *RingBuffer) {
	out := NewRingBuffer(4096)
	return NewMixer(out.Write), out
}

// fillPeer 向 peer 输入环写入恒定值帧。
func fillPeer(p *PeerInput, v int16, frames int) {
	pcm := make([]int16, FrameSize)
	for i := range pcm {
		pcm[i] = v
	}
	for ; frames > 0; frames-- {
		if n := p.Write(pcm); n != FrameSize {
			panic("fillPeer: short write")
		}
	}
}

func readFrame(t *testing.T, r *RingBuffer) []int16 {
	t.Helper()
	out := make([]int16, FrameSize)
	if n := r.Read(out); n != FrameSize {
		t.Fatalf("read %d samples, want %d", n, FrameSize)
	}
	return out
}

func TestMixerSumTwoPeers(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	b := m.AddPeer("b")
	fillPeer(a, 1000, 1)
	fillPeer(b, 2500, 1)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 3500 {
			t.Fatalf("sample %d = %d, want 3500", i, v)
		}
	}
	if got := m.FramesMixed.Load(); got != 1 {
		t.Fatalf("FramesMixed = %d, want 1", got)
	}
	if got := m.ClippedSamples.Load(); got != 0 {
		t.Fatalf("ClippedSamples = %d, want 0", got)
	}
}

func TestMixerClipping(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	b := m.AddPeer("b")
	fillPeer(a, 20000, 1)
	fillPeer(b, 20000, 1)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 32767 {
			t.Fatalf("sample %d = %d, want 32767 (clipped)", i, v)
		}
	}
	if got := m.ClippedSamples.Load(); got != FrameSize {
		t.Fatalf("ClippedSamples = %d, want %d", got, FrameSize)
	}
}

func TestMixerNegativeClipping(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	b := m.AddPeer("b")
	fillPeer(a, -20000, 1)
	fillPeer(b, -20000, 1)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != -32768 {
			t.Fatalf("sample %d = %d, want -32768 (clipped)", i, v)
		}
	}
}

func TestMixerGain(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	b := m.AddPeer("b")
	a.SetGain(0.5)
	fillPeer(a, 4000, 1)
	fillPeer(b, 1000, 1)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 3000 { // 4000*0.5 + 1000
			t.Fatalf("sample %d = %d, want 3000", i, v)
		}
	}
}

// TestMixerMasterGain 扬声器总音量：master gain 在求和后截断前施加（v0.1.2）。
func TestMixerMasterGain(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	fillPeer(a, 2000, 1)
	m.SetMasterGain(0.5)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 1000 {
			t.Fatalf("sample %d = %d, want 1000（2000×0.5）", i, v)
		}
	}
	if m.MasterGain() != 0.5 {
		t.Fatalf("MasterGain = %v", m.MasterGain())
	}
}

func TestMixerMute(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	b := m.AddPeer("b")
	a.SetMuted(true)
	fillPeer(a, 5000, 1)
	fillPeer(b, 1000, 1)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 1000 {
			t.Fatalf("sample %d = %d, want 1000 (a muted)", i, v)
		}
	}
	// 解除静音后不应播出陈旧音频（静音期间已排空）。
	a.SetMuted(false)
	m.Tick()
	frame = readFrame(t, out)
	for i, v := range frame {
		if v != 0 {
			t.Fatalf("sample %d = %d, want 0 (stale drained)", i, v)
		}
	}
}

func TestMixerUnderrun(t *testing.T) {
	m, out := newTestMixer()
	m.AddPeer("silent")

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 0 {
			t.Fatalf("sample %d = %d, want 0 (underrun)", i, v)
		}
	}
	p := m.Peer("silent")
	if got := p.UnderrunFrames.Load(); got != 1 {
		t.Fatalf("UnderrunFrames = %d, want 1", got)
	}
}

func TestMixerDeafen(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	fillPeer(a, 5000, 1)
	m.SetDeafen(true)

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 0 {
			t.Fatalf("sample %d = %d, want 0 (deafen)", i, v)
		}
	}
	if got := a.ring.Available(); got != 0 {
		t.Fatalf("deafen should drain peer ring, %d samples left", got)
	}
}

func TestMixerRemovePeer(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	fillPeer(a, 5000, 1)
	m.RemovePeer("a")
	m.RemovePeer("a") // 幂等

	m.Tick()

	frame := readFrame(t, out)
	for i, v := range frame {
		if v != 0 {
			t.Fatalf("sample %d = %d, want 0 (peer removed)", i, v)
		}
	}
	if got := m.Peer("a"); got != nil {
		t.Fatalf("Peer(a) after remove = %v, want nil", got)
	}
	if ids := m.PeerIDs(); len(ids) != 0 {
		t.Fatalf("PeerIDs = %v, want empty", ids)
	}
}

func TestMixerAddPeerIdempotent(t *testing.T) {
	m, _ := newTestMixer()
	a1 := m.AddPeer("a")
	a2 := m.AddPeer("a")
	if a1 != a2 {
		t.Fatal("AddPeer not idempotent")
	}
	if ids := m.PeerIDs(); len(ids) != 1 {
		t.Fatalf("PeerIDs len = %d, want 1", len(ids))
	}
}

// TestMixerStartStop 节拍 goroutine 生命周期。
func TestMixerStartStop(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	fillPeer(a, 1000, 4)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Start(); err == nil {
		t.Fatal("second Start should fail")
	}
	time.Sleep(60 * time.Millisecond) // 等几拍
	m.Stop()
	m.Stop() // 幂等
	if got := m.FramesMixed.Load(); got == 0 {
		t.Fatal("no frames mixed")
	}
	if out.Available() == 0 {
		t.Fatal("no output written")
	}
}

// TestMixerDuckSmoothing 优先发言 ducking（#45）：独立乘数与用户增益叠乘，
// attack/release 分拍平滑，结束后恢复原用户音量。
func TestMixerDuckSmoothing(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	a.SetGain(0.5)
	a.SetDucked(true)
	if !a.Ducked() {
		t.Fatal("SetDucked(true) 后目标应为压低")
	}

	// attack 第一拍：乘数从 1.0 迈出一步（非瞬间跳变）。
	fillPeer(a, 4000, 1)
	m.Tick()
	first := readFrame(t, out)[0]
	firstDuck := 1 - (1-PriorityDuckGain)/duckAttackTicks
	wantFirst := int16(float64(4000) * 0.5 * firstDuck)
	if first != wantFirst {
		t.Fatalf("首拍 duck 采样 = %d，期望 %d（平滑起点）", first, wantFirst)
	}

	// 再走满 attackTicks-1 拍，应到达 4000×0.5×0.35=700。
	for i := 1; i < duckAttackTicks; i++ {
		fillPeer(a, 4000, 1)
		m.Tick()
		_ = readFrame(t, out)
	}
	fillPeer(a, 4000, 1)
	m.Tick()
	if got := readFrame(t, out)[0]; got != 700 {
		t.Fatalf("attack 完成后采样 = %d，期望 700（4000×0.5×0.35）", got)
	}
	if a.Gain() != 0.5 {
		t.Fatalf("duck 不得覆盖用户音量，Gain = %v", a.Gain())
	}

	// release：20 拍后恢复 4000×0.5=2000。
	a.SetDucked(false)
	if a.Ducked() {
		t.Fatal("SetDucked(false) 后目标应恢复")
	}
	for i := 0; i < duckReleaseTicks; i++ {
		fillPeer(a, 4000, 1)
		m.Tick()
		_ = readFrame(t, out)
	}
	fillPeer(a, 4000, 1)
	m.Tick()
	if got := readFrame(t, out)[0]; got != 2000 {
		t.Fatalf("release 完成后采样 = %d，期望 2000（用户增益恢复）", got)
	}
}

// TestMixerDuckZeroAlloc duck 平滑不得给 Tick 增加堆分配。
func TestMixerDuckZeroAlloc(t *testing.T) {
	m, out := newTestMixer()
	a := m.AddPeer("a")
	a.SetDucked(true)
	buf := make([]int16, FrameSize)
	allocs := testing.AllocsPerRun(100, func() {
		m.Tick() // 欠载路径也会推进 duck 平滑
		_ = out.Read(buf)
	})
	if allocs != 0 {
		t.Fatalf("duck Tick 每拍分配 %v，期望 0", allocs)
	}
}

// BenchmarkMixerTick 4 路 peer 混音热点：必须 0 allocs/op。
func BenchmarkMixerTick(b *testing.B) {
	out := NewRingBuffer(1 << 20)
	m := NewMixer(out.Write)
	peers := []*PeerInput{
		m.AddPeer("a"), m.AddPeer("b"), m.AddPeer("c"), m.AddPeer("d"),
	}
	pcm := make([]int16, FrameSize)
	for i := range pcm {
		pcm[i] = int16(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range peers {
			p.ring.Write(pcm) // 环满后自然短写，Tick 每拍恒定读 480
		}
		m.Tick()
	}
}

// BenchmarkMixerTickUnderrun 全部欠载（纯零填充路径）。
func BenchmarkMixerTickUnderrun(b *testing.B) {
	out := NewRingBuffer(1 << 20)
	m := NewMixer(out.Write)
	m.AddPeer("a")
	m.AddPeer("b")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Tick()
	}
}
