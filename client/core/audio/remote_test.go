package audio

import (
	"math"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/codec"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// encodeSinePackets 编码 n 个 20ms 的 440Hz 正弦 Opus 包（对端下行仿真）。
func encodeSinePackets(t testing.TB, n int) [][]byte {
	t.Helper()
	enc, err := codec.NewEncoder(SampleRate, 1, codec.AppVoIP)
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	defer enc.Close()
	if err := enc.SetBitrate(32000); err != nil {
		t.Fatalf("set bitrate: %v", err)
	}
	pcm := make([]int16, OpusFrameSize)
	buf := make([]byte, 4000)
	var phase float64
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		for j := range pcm {
			phase += 2 * math.Pi * 440 / SampleRate
			pcm[j] = int16(12000 * math.Sin(phase))
		}
		m, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		out = append(out, append([]byte(nil), buf[:m]...))
	}
	return out
}

// newTestRemotePeer 直接构造单路远端（不带 track），解码拍由测试同步驱动。
func newTestRemotePeer(t testing.TB, id string, m *Mixer) *remotePeer {
	t.Helper()
	requireOpus(t)
	dec, err := codec.NewDecoder(SampleRate, 1)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	rp := &remotePeer{id: id, jb: rtc.NewJitterBuffer(), dec: dec, done: make(chan struct{})}
	rp.input = m.AddPeer(id)
	t.Cleanup(func() {
		select { // 生命周期用例可能已经 stop 过（done 关闭）
		case <-rp.done:
		default:
			rp.stop()
		}
	})
	return rp
}

// jitterBase 与 rtc 包测试同款的固定到达时刻基准（避免真实时钟参与）。
var remoteArrivalBase = time.UnixMilli(1_700_000_000_000)

func remoteArrival(seq int) time.Time {
	return remoteArrivalBase.Add(time.Duration(seq) * JitterPopInterval)
}

// encodeRampedSinePackets 编码 n 个 20ms 包：440Hz 正弦，逐帧提升幅度，
// 开启带内 FEC 并告知 20% 丢包率。幅度递增让「FEC 恢复真实丢帧」与
// 「PLC 沿用上一帧包络」的能量差异可判定。
func encodeRampedSinePackets(t testing.TB, n int) [][]byte {
	return encodeRampedSineFrames(t, n, OpusFrameSize)
}

// encodeRampedSine10msPackets 同 20ms 版，但帧长 10ms（480 采样，#116）。
func encodeRampedSine10msPackets(t testing.TB, n int) [][]byte {
	return encodeRampedSineFrames(t, n, FrameSize)
}

func encodeRampedSineFrames(t testing.TB, n, frameSize int) [][]byte {
	t.Helper()
	enc, err := codec.NewEncoder(SampleRate, 1, codec.AppVoIP)
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	defer enc.Close()
	if err := enc.SetBitrate(32000); err != nil {
		t.Fatalf("set bitrate: %v", err)
	}
	if err := enc.SetInBandFEC(true); err != nil {
		t.Fatalf("set fec: %v", err)
	}
	if err := enc.SetPacketLossPerc(20); err != nil {
		t.Fatalf("set packet loss: %v", err)
	}
	pcm := make([]int16, frameSize)
	buf := make([]byte, 4000)
	var phase float64
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		amp := 4000 + 1000*float64(i)
		for j := range pcm {
			phase += 2 * math.Pi * 440 / SampleRate
			pcm[j] = int16(amp * math.Sin(phase))
		}
		m, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		out = append(out, append([]byte(nil), buf[:m]...))
	}
	return out
}

// TestRemoteDecodeOnce 预填满后逐拍弹出：正常帧解码 960 采样写入混音输入环，
// 解码信号必须为非静音正弦。
func TestRemoteDecodeOnce(t *testing.T) {
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	pkts := encodeSinePackets(t, 6)

	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
	}
	for i := 0; i < 3; i++ {
		rp.decodeOnce()
	}

	if got := rp.Stats.FramesPopped.Load(); got != 3 {
		t.Fatalf("FramesPopped = %d, want 3", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 0 {
		t.Fatalf("PLCFrames = %d, want 0", got)
	}
	if got := rp.Stats.FECFrames.Load(); got != 0 {
		t.Fatalf("FECFrames = %d, want 0", got)
	}
	if got := rp.input.WrittenSamples.Load(); got != 3*OpusFrameSize {
		t.Fatalf("WrittenSamples = %d, want %d", got, 3*OpusFrameSize)
	}

	buf := make([]int16, 3*OpusFrameSize)
	if n := rp.input.ring.Read(buf); n != len(buf) {
		t.Fatalf("ring read %d, want %d", n, len(buf))
	}
	if r := rmsInt16(buf); r < 1000 {
		t.Fatalf("decoded RMS %.0f too low, sine should survive", r)
	}
}

// TestRemoteDecodePLC 无后继包的空洞帧必须回落 PLC 并照常合成 960 采样送混音。
func TestRemoteDecodePLC(t *testing.T) {
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	pkts := encodeSinePackets(t, 3)

	// 100,101,102 正常；103 丢失且无后继包（断流）。
	rp.jb.Push(100, pkts[0], remoteArrival(100))
	rp.jb.Push(101, pkts[1], remoteArrival(101))
	rp.jb.Push(102, pkts[2], remoteArrival(102))

	for i := 0; i < 4; i++ {
		rp.decodeOnce()
	}

	if got := rp.Stats.FramesPopped.Load(); got != 3 {
		t.Fatalf("FramesPopped = %d, want 3", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 1 {
		t.Fatalf("PLCFrames = %d, want 1", got)
	}
	if got := rp.Stats.FECFrames.Load(); got != 0 {
		t.Fatalf("FECFrames = %d, want 0", got)
	}
	if got := rp.input.WrittenSamples.Load(); got != 4*OpusFrameSize {
		t.Fatalf("WrittenSamples = %d, want %d (PLC 帧同样 960)", got, 4*OpusFrameSize)
	}
}

// TestRemoteDecodeFECRecoversLostFrame 丢帧时若后继包已在 jitter 槽内，用后继包
// 的带内 FEC 恢复该帧：恢复信号能量应接近真实帧、明显优于 PLC；后继包随后
// 仍按正常路径解码（未被消费）。对照 PLC 走独立解码器，初始状态一致。
func TestRemoteDecodeFECRecoversLostFrame(t *testing.T) {
	pkts := encodeRampedSinePackets(t, 8)
	drain := make([]int16, OpusFrameSize)

	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
	}
	for i := 0; i < 3; i++ {
		rp.decodeOnce()
		rp.input.ring.Read(drain) // 排空正常帧
	}

	// 103 丢失、104 已到：FEC 恢复 103。
	if !codec.HasLBRR(pkts[4]) {
		t.Fatal("FEC 用例前提不成立：后继包 104 不含 LBRR")
	}
	rp.jb.Push(104, pkts[4], remoteArrival(104))
	rp.decodeOnce()
	if n := rp.input.ring.Read(drain); n != OpusFrameSize {
		t.Fatalf("FEC frame produced %d samples, want %d", n, OpusFrameSize)
	}
	fecRMS := rmsInt16(drain)
	if got := rp.Stats.FECFrames.Load(); got != 1 {
		t.Fatalf("FECFrames = %d, want 1", got)
	}
	if got := rp.Stats.DREDFrames.Load(); got != 0 {
		t.Fatalf("DREDFrames = %d, want 0（FEC 可用时应优先 FEC）", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 0 {
		t.Fatalf("PLCFrames = %d, want 0", got)
	}

	// 后继 104 正常解码，解码器状态未被 FEC 破坏。
	rp.decodeOnce()
	if n := rp.input.ring.Read(drain); n != OpusFrameSize {
		t.Fatalf("successor frame produced %d samples, want %d", n, OpusFrameSize)
	}
	if got := rp.Stats.FramesPopped.Load(); got != 4 {
		t.Fatalf("FramesPopped = %d, want 4 (3 prefill + successor)", got)
	}
	if r := rmsInt16(drain); r < 3000 {
		t.Fatalf("successor decoded RMS %.0f too low", r)
	}

	// PLC 对照：同样的解码器历史，103 丢失且无后继。
	m2, _ := newTestMixer()
	rp2 := newTestRemotePeer(t, "b", m2)
	for i := 0; i < 3; i++ {
		rp2.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
	}
	for i := 0; i < 3; i++ {
		rp2.decodeOnce()
		rp2.input.ring.Read(drain)
	}
	rp2.decodeOnce()
	if n := rp2.input.ring.Read(drain); n != OpusFrameSize {
		t.Fatalf("PLC frame produced %d samples, want %d", n, OpusFrameSize)
	}
	plcRMS := rmsInt16(drain)
	if got := rp2.Stats.PLCFrames.Load(); got != 1 {
		t.Fatalf("control PLCFrames = %d, want 1", got)
	}

	if fecRMS <= plcRMS*1.15 {
		t.Fatalf("FEC RMS %.0f not clearly better than PLC RMS %.0f", fecRMS, plcRMS)
	}
}

// TestRemoteDecodeFECRecoversLostFrame10ms 10ms 频道（#116）：丢帧用后继包的
// 带内 FEC 恢复，输出必须按当前帧长 480 截断（不能沿用整块 960），FECFrames
// 增加且后继包随后仍正常解码。
func TestRemoteDecodeFECRecoversLostFrame10ms(t *testing.T) {
	pkts := encodeRampedSine10msPackets(t, 10)
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	rp.frameMS.Store(FrameMS10)
	if err := rp.jb.SetFrameMS(FrameMS10); err != nil {
		t.Fatal(err)
	}
	arrival := func(seq int) time.Time {
		return remoteArrivalBase.Add(time.Duration(seq) * 10 * time.Millisecond)
	}
	drain := make([]int16, FrameSize)

	// 10ms 帧的预填深度是 6 包（60ms），比 20ms 的 3 包多一倍。
	for i := 0; i < 6; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], arrival(100+i))
	}
	for i := 0; i < 6; i++ {
		rp.decodeOnce()
		if n := rp.input.ring.Read(drain); n != FrameSize {
			t.Fatalf("normal frame produced %d samples, want %d", n, FrameSize)
		}
	}

	// 106 丢失、107 已到：FEC 恢复 106。
	if !codec.HasLBRR(pkts[7]) {
		t.Fatal("10ms FEC 用例前提不成立：后继包 107 不含 LBRR")
	}
	rp.jb.Push(107, pkts[7], arrival(107))
	rp.decodeOnce()
	if n := rp.input.ring.Read(drain); n != FrameSize {
		t.Fatalf("FEC frame produced %d samples, want %d", n, FrameSize)
	}
	if got := rp.Stats.FECFrames.Load(); got != 1 {
		t.Fatalf("FECFrames = %d, want 1", got)
	}
	if got := rp.Stats.DREDFrames.Load(); got != 0 {
		t.Fatalf("DREDFrames = %d, want 0（FEC 可用时应优先 FEC）", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 0 {
		t.Fatalf("PLCFrames = %d, want 0", got)
	}

	// 后继 107 正常解码（480 采样），未被 FEC 消费。
	rp.decodeOnce()
	if n := rp.input.ring.Read(drain); n != FrameSize {
		t.Fatalf("successor frame produced %d samples, want %d", n, FrameSize)
	}
	if got := rp.Stats.FramesPopped.Load(); got != 7 {
		t.Fatalf("FramesPopped = %d, want 7 (6 prefill + successor)", got)
	}
}

// encodeDREDPackets 编码 n 个 20ms 包：440Hz 正弦逐帧提升幅度，开启 DRED
// （200ms）并告知 20% 丢包率，但关闭带内 FEC——模拟编码端未提供 LBRR、只能靠
// DRED 恢复的弱网场景（#44）。
func encodeDREDPackets(t testing.TB, n int) [][]byte {
	t.Helper()
	enc, err := codec.NewEncoder(SampleRate, 1, codec.AppVoIP)
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	defer enc.Close()
	if err := enc.SetBitrate(32000); err != nil {
		t.Fatalf("set bitrate: %v", err)
	}
	if err := enc.SetInBandFEC(false); err != nil {
		t.Fatalf("disable fec: %v", err)
	}
	if err := enc.SetDRED(20); err != nil {
		t.Fatalf("set dred: %v", err)
	}
	if err := enc.SetPacketLossPerc(20); err != nil {
		t.Fatalf("set packet loss: %v", err)
	}
	pcm := make([]int16, OpusFrameSize)
	buf := make([]byte, 4000)
	var phase float64
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		amp := 8000 + 1000*float64(i)
		for j := range pcm {
			phase += 2 * math.Pi * 440 / SampleRate
			pcm[j] = int16(amp * math.Sin(phase))
		}
		m, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		out = append(out, append([]byte(nil), buf[:m]...))
	}
	return out
}

// TestRemoteDecodeDREDRecoversLostFrame FEC 不可用时（编码端未开 FEC），丢帧
// 由后继包的 DRED 冗余恢复：DREDFrames 增加、输出非静音并写入混音输入环；
// 后继包随后仍按正常路径解码。
func TestRemoteDecodeDREDRecoversLostFrame(t *testing.T) {
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	pkts := encodeDREDPackets(t, 8)
	drain := make([]int16, OpusFrameSize)

	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
	}
	for i := 0; i < 3; i++ {
		rp.decodeOnce()
		rp.input.ring.Read(drain) // 排空正常帧
	}

	// 103 丢失、104 已到：FEC 无 LBRR 可用，DRED 从 104 恢复 103。
	if codec.HasLBRR(pkts[4]) {
		t.Fatal("DRED 用例前提不成立：FEC 已关闭，后继包不应带 LBRR")
	}
	rp.jb.Push(104, pkts[4], remoteArrival(104))
	rp.decodeOnce()
	n := rp.input.ring.Read(drain)
	if n != OpusFrameSize {
		t.Fatalf("DRED frame produced %d samples, want %d", n, OpusFrameSize)
	}
	if got := rp.Stats.DREDFrames.Load(); got != 1 {
		t.Fatalf("DREDFrames = %d, want 1", got)
	}
	if got := rp.Stats.FECFrames.Load(); got != 0 {
		t.Fatalf("FECFrames = %d, want 0", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 0 {
		t.Fatalf("PLCFrames = %d, want 0（DRED 已恢复）", got)
	}
	if r := rmsInt16(drain); r < 1000 {
		t.Fatalf("DRED 恢复输出接近静音（RMS %.0f）", r)
	}

	// 后继 104 正常解码，DRED 未破坏解码器状态。
	rp.decodeOnce()
	if n := rp.input.ring.Read(drain); n != OpusFrameSize {
		t.Fatalf("successor frame produced %d samples, want %d", n, OpusFrameSize)
	}
	if got := rp.Stats.FramesPopped.Load(); got != 4 {
		t.Fatalf("FramesPopped = %d, want 4 (3 prefill + successor)", got)
	}
	if r := rmsInt16(drain); r < 1000 {
		t.Fatalf("successor decoded RMS %.0f too low", r)
	}
}

// TestRemoteDecodeFECFallsBackToPLC 连续丢包时，紧邻后继缺失的丢帧回落 PLC；
// 后继到达后仍可用它恢复再前一帧。全程每拍都写出 960 采样。
func TestRemoteDecodeFECFallsBackToPLC(t *testing.T) {
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	pkts := encodeRampedSinePackets(t, 8)
	drain := make([]int16, OpusFrameSize)

	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
	}
	for i := 0; i < 3; i++ {
		rp.decodeOnce()
		rp.input.ring.Read(drain)
	}

	// 103、104 连续丢失，105 已到。
	rp.jb.Push(105, pkts[5], remoteArrival(105))
	rp.decodeOnce() // 103：紧邻后继 104 未到 → PLC
	if got := rp.Stats.PLCFrames.Load(); got != 1 {
		t.Fatalf("PLCFrames after first consecutive loss = %d, want 1", got)
	}
	if got := rp.Stats.FECFrames.Load(); got != 0 {
		t.Fatalf("FECFrames after first consecutive loss = %d, want 0", got)
	}
	rp.decodeOnce() // 104：后继 105 已到 → FEC
	if got := rp.Stats.FECFrames.Load(); got != 1 {
		t.Fatalf("FECFrames after second loss = %d, want 1", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 1 {
		t.Fatalf("PLCFrames after second loss = %d, want 1", got)
	}
	rp.decodeOnce() // 105：正常帧
	if got := rp.Stats.FramesPopped.Load(); got != 4 {
		t.Fatalf("FramesPopped = %d, want 4", got)
	}
	if got := rp.input.WrittenSamples.Load(); got != 6*OpusFrameSize {
		t.Fatalf("WrittenSamples = %d, want %d", got, 6*OpusFrameSize)
	}
}

// TestRemoteDecodeError 解码失败计数且不向混音环写脏数据。
func TestRemoteDecodeError(t *testing.T) {
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	pkts := encodeSinePackets(t, 3)
	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
	}
	_ = rp.dec.Close() // 解码器不可用

	rp.decodeOnce()

	if got := rp.Stats.DecodeErrors.Load(); got != 1 {
		t.Fatalf("DecodeErrors = %d, want 1", got)
	}
	if got := rp.input.WrittenSamples.Load(); got != 0 {
		t.Fatalf("WrittenSamples = %d, want 0", got)
	}
}

// encode10msPackets 编码 n 个 10ms（480 采样）的 440Hz 正弦 Opus 包（#46）。
func encode10msPackets(t testing.TB, n int) [][]byte {
	t.Helper()
	enc, err := codec.NewEncoder(SampleRate, 1, codec.AppVoIP)
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	defer enc.Close()
	if err := enc.SetBitrate(32000); err != nil {
		t.Fatalf("set bitrate: %v", err)
	}
	pcm := make([]int16, FrameSize)
	buf := make([]byte, 4000)
	var phase float64
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		for j := range pcm {
			phase += 2 * math.Pi * 440 / SampleRate
			pcm[j] = int16(12000 * math.Sin(phase))
		}
		m, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		out = append(out, append([]byte(nil), buf[:m]...))
	}
	return out
}

// TestRemoteDecode10ms 10ms 频道（#46）：正常帧解码 480 采样，PLC 同样合成
// 480 采样（不能沿用上一包的 960），写出总量按 480/拍累计。
func TestRemoteDecode10ms(t *testing.T) {
	m, _ := newTestMixer()
	rp := newTestRemotePeer(t, "a", m)
	rp.frameMS.Store(FrameMS10)
	if err := rp.jb.SetFrameMS(FrameMS10); err != nil {
		t.Fatal(err)
	}
	if got := rp.frameSamples(); got != FrameSize {
		t.Fatalf("frameSamples = %d, want %d", got, FrameSize)
	}
	pkts := encode10msPackets(t, 6)
	// 10ms 到达间隔（与帧长一致），避免抖动估计被测试夹具放大。
	arrival10 := func(seq int) time.Time {
		return remoteArrivalBase.Add(time.Duration(seq) * 10 * time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], arrival10(100+i))
	}
	for i := 0; i < 7; i++ {
		rp.decodeOnce()
	}
	if got := rp.Stats.FramesPopped.Load(); got != 6 {
		t.Fatalf("FramesPopped = %d, want 6", got)
	}
	if got := rp.Stats.PLCFrames.Load(); got != 1 {
		t.Fatalf("PLCFrames = %d, want 1", got)
	}
	if got := rp.input.WrittenSamples.Load(); got != 7*FrameSize {
		t.Fatalf("WrittenSamples = %d, want %d", got, 7*FrameSize)
	}
	buf := make([]int16, 6*FrameSize)
	if n := rp.input.ring.Read(buf); n != len(buf) {
		t.Fatalf("ring read %d, want %d", n, len(buf))
	}
	if r := rmsInt16(buf); r < 1000 {
		t.Fatalf("decoded RMS %.0f too low", r)
	}
}

// TestRemoteManagerFrameMS 管理器帧长写入与热切换；非法值拒绝。
func TestRemoteManagerFrameMS(t *testing.T) {
	m, _ := newTestMixer()
	rm := NewRemoteManager(m, nil)
	if got := rm.FrameMS(); got != FrameMS20 {
		t.Fatalf("默认帧长 = %d, want %d", got, FrameMS20)
	}
	if err := rm.SetFrameMS(FrameMS10); err != nil || rm.FrameMS() != FrameMS10 {
		t.Fatalf("SetFrameMS(10): %v, got %d", err, rm.FrameMS())
	}
	if err := rm.SetFrameMS(15); err == nil {
		t.Fatal("非法帧长应报错")
	}
	if got := rm.FrameMS(); got != FrameMS10 {
		t.Fatalf("非法值不应改变帧长: %d", got)
	}
}

// TestRemoteLinkStats 诊断快照（#95）：抖动统计、链路计数与混音输入环占用
// 与解码路径一致；未知 peer 返回 false。
func TestRemoteLinkStats(t *testing.T) {
	m, _ := newTestMixer()
	rm := NewRemoteManager(m, nil)
	rp := newTestRemotePeer(t, "a", m)
	rm.peers["a"] = rp
	pkts := encodeSinePackets(t, 3)
	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(100+i), pkts[i], remoteArrival(100+i))
		rp.Stats.RTPReceived.Add(1) // 模拟 readLoop 计数
	}
	for i := 0; i < 3; i++ {
		rp.decodeOnce()
	}

	ls, ok := rm.LinkStats("a")
	if !ok {
		t.Fatal("LinkStats(a) not found")
	}
	if ls.RTPReceived != 3 || ls.FramesPopped != 3 || ls.PLCFrames != 0 || ls.FECFrames != 0 || ls.DREDFrames != 0 {
		t.Fatalf("link counters = %+v, want rtp=3 popped=3 plc=0 fec=0 dred=0", ls)
	}
	if ls.RingAvailable != 3*OpusFrameSize || ls.WrittenSamples != 3*OpusFrameSize {
		t.Fatalf("ring = %d/%d, want %d/%d", ls.RingAvailable, ls.WrittenSamples, 3*OpusFrameSize, 3*OpusFrameSize)
	}
	if !ls.Jitter.Started || ls.Jitter.TargetPackets < 3 || ls.Jitter.Buffered != 0 {
		t.Fatalf("jitter stats = %+v, want started, target>=3, buffered=0", ls.Jitter)
	}
	if _, ok := rm.LinkStats("missing"); ok {
		t.Fatal("LinkStats(missing) should be false")
	}
}

// TestRemoteManagerRemove peer 离开：链路与混音输入一并销毁，幂等。
func TestRemoteManagerRemove(t *testing.T) {
	m, _ := newTestMixer()
	rm := NewRemoteManager(m, nil)
	rp := newTestRemotePeer(t, "x", m)
	rm.peers["x"] = rp

	if ids := rm.PeerIDs(); len(ids) != 1 || ids[0] != "x" {
		t.Fatalf("PeerIDs = %v, want [x]", ids)
	}

	rm.RemovePeer("x")
	rm.RemovePeer("x") // 幂等
	if ids := rm.PeerIDs(); len(ids) != 0 {
		t.Fatalf("PeerIDs after remove = %v, want empty", ids)
	}
	if m.Peer("x") != nil {
		t.Fatal("mixer input should be destroyed on peer leave")
	}

	// RemoveAll 清房同样销毁输入。
	rpY := newTestRemotePeer(t, "y", m)
	rpZ := newTestRemotePeer(t, "z", m)
	rm.peers["y"] = rpY
	rm.peers["z"] = rpZ
	rm.RemoveAll()
	rm.RemoveAll() // 幂等
	if ids := rm.PeerIDs(); len(ids) != 0 {
		t.Fatalf("PeerIDs after RemoveAll = %v, want empty", ids)
	}
	if m.Peer("y") != nil || m.Peer("z") != nil {
		t.Fatal("mixer inputs should be destroyed on RemoveAll")
	}
}

// TestRemoteRemoveIfIdentity track EOF 自愈拆除不得误删重协商后的新链路。
func TestRemoteRemoveIfIdentity(t *testing.T) {
	m, _ := newTestMixer()
	rm := NewRemoteManager(m, nil)
	old := newTestRemotePeer(t, "x", m)
	rm.peers["x"] = old

	// 重协商：新实例接管同名 peer（输入环复用）。
	fresh := newTestRemotePeer(t, "x", m)
	rm.peers["x"] = fresh

	rm.removeIf(old) // 旧实例 EOF 迟到：不得拆除新链路
	if ids := rm.PeerIDs(); len(ids) != 1 || ids[0] != "x" {
		t.Fatalf("PeerIDs after stale removeIf = %v, want [x]", ids)
	}
	if m.Peer("x") == nil {
		t.Fatal("stale removeIf destroyed the shared mixer input")
	}

	rm.removeIf(fresh) // 新实例 EOF：正常拆除
	if ids := rm.PeerIDs(); len(ids) != 0 {
		t.Fatalf("PeerIDs after removeIf = %v, want empty", ids)
	}
	if m.Peer("x") != nil {
		t.Fatal("mixer input should be destroyed by removeIf")
	}
}

// TestStateTable 对端语音状态表：增量/全量/移除与回调语义。
func TestStateTable(t *testing.T) {
	st := NewStateTable()
	var changes, removes []string
	st.OnChange = func(id string, _ PeerVoiceState) { changes = append(changes, id) }
	st.OnRemove = func(id string) { removes = append(removes, id) }

	speak, mute := true, false
	st.Apply("a", &speak, &mute, nil)
	if v, ok := st.Get("a"); !ok || !v.Speaking || v.Mute || v.Deafen {
		t.Fatalf("a = %+v ok=%v, want speaking", v, ok)
	}
	deafen := true
	st.Apply("a", nil, nil, &deafen) // 增量：未带字段不变
	if v, _ := st.Get("a"); !v.Speaking || !v.Deafen {
		t.Fatalf("a after delta = %+v, want speaking+deafen", v)
	}
	st.Set("b", PeerVoiceState{Mute: true})
	if v, ok := st.Get("b"); !ok || !v.Mute || v.Speaking {
		t.Fatalf("b = %+v ok=%v, want muted", v, ok)
	}
	if snap := st.Snapshot(); len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}

	st.Remove("a")
	if _, ok := st.Get("a"); ok {
		t.Fatal("a should be gone")
	}
	st.Remove("a") // 幂等：不重复回调
	if len(removes) != 1 {
		t.Fatalf("OnRemove fired %d times, want 1", len(removes))
	}
	if len(changes) != 3 { // a Apply ×2 + b Set
		t.Fatalf("OnChange fired %d times, want 3", len(changes))
	}
}

// TestStateTableClear 退房清空：条目全删、OnRemove 逐条回调、清空后可继续使用。
func TestStateTableClear(t *testing.T) {
	st := NewStateTable()
	var removes []string
	st.OnRemove = func(id string) { removes = append(removes, id) }
	st.Set("a", PeerVoiceState{Speaking: true})
	st.Set("b", PeerVoiceState{Mute: true})

	st.Clear()
	if got := len(st.Snapshot()); got != 0 {
		t.Fatalf("Snapshot after Clear = %d, want 0", got)
	}
	seen := make(map[string]bool, len(removes))
	for _, id := range removes {
		seen[id] = true
	}
	if len(removes) != 2 || !seen["a"] || !seen["b"] {
		t.Fatalf("OnRemove after Clear = %v, want a and b", removes)
	}

	st.Clear() // 空表幂等：不重复回调
	if len(removes) != 2 {
		t.Fatalf("OnRemove after empty Clear = %d, want 2", len(removes))
	}

	st.Set("c", PeerVoiceState{Deafen: true})
	if v, ok := st.Get("c"); !ok || !v.Deafen {
		t.Fatalf("c after Clear = %+v ok=%v, want deafen", v, ok)
	}
}

// newTestSFUPC 构造模拟 SFU 的 pion PeerConnection（opus pt=111，与 rtc.Peer 对齐）。
func newTestSFUPC(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: rtc.OpusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("sfu register codec: %v", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		t.Fatalf("sfu interceptors: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir)).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("sfu new pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// TestRemoteHandleTrackLoopback 本机 loopback 全链路：fake SFU 下行真实 Opus
// RTP → rtc.Peer OnTrack → RemoteManager.HandleTrack → jitter → 解码 → 混音输入；
// peer 离开必须拆除链路与混音输入。
func TestRemoteHandleTrackLoopback(t *testing.T) {
	requireOpus(t)
	m, _ := newTestMixer()
	rm := NewRemoteManager(m, nil)
	t.Cleanup(rm.RemoveAll)

	sfu := newTestSFUPC(t)

	localSDP := make(chan webrtc.SessionDescription, 4)
	localICE := make(chan webrtc.ICECandidateInit, 64)
	peer, err := rtc.NewPeer(rtc.PeerConfig{
		StreamID: "client-A",
		Callbacks: rtc.PeerCallbacks{
			OnLocalSDP: func(sdp webrtc.SessionDescription) { localSDP <- sdp },
			OnLocalICE: func(c webrtc.ICECandidateInit) { localICE <- c },
			OnTrack:    rm.HandleTrack,
		},
	})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	// 双向 trickle（管道回调里不触碰 t：测试结束后 pion 仍可能触发）。
	sfu.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = peer.HandleRemoteICE(c.ToJSON())
	})
	iceDone := make(chan struct{})
	go func() {
		defer close(iceDone)
		for c := range localICE {
			_ = sfu.AddICECandidate(c)
		}
	}()
	defer func() { close(localICE); <-iceDone }()

	// 初始 offer/answer。
	if err := peer.Offer(); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if err := sfu.SetRemoteDescription(<-localSDP); err != nil {
		t.Fatalf("sfu set remote offer: %v", err)
	}
	answer, err := sfu.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("sfu create answer: %v", err)
	}
	if err := sfu.SetLocalDescription(answer); err != nil {
		t.Fatalf("sfu set local answer: %v", err)
	}
	if err := peer.HandleRemoteSDP(answer); err != nil {
		t.Fatalf("HandleRemoteSDP(answer): %v", err)
	}

	// ICE 连通。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) &&
		(peer.ConnectionState() != webrtc.PeerConnectionStateConnected ||
			sfu.ConnectionState() != webrtc.PeerConnectionStateConnected) {
		time.Sleep(20 * time.Millisecond)
	}
	if peer.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatalf("not connected: client=%s sfu=%s", peer.ConnectionState(), sfu.ConnectionState())
	}

	// SFU 加下行轨并重协商：TrackID/StreamID 按服务端约定（room.go ensureRoute）。
	downTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio-peer-B", "litetalk-test",
	)
	if err != nil {
		t.Fatalf("new down track: %v", err)
	}
	if _, err := sfu.AddTrack(downTrack); err != nil {
		t.Fatalf("sfu add track: %v", err)
	}
	reneg, err := sfu.CreateOffer(nil)
	if err != nil {
		t.Fatalf("sfu reneg offer: %v", err)
	}
	if err := sfu.SetLocalDescription(reneg); err != nil {
		t.Fatalf("sfu set local reneg offer: %v", err)
	}
	if err := peer.HandleRemoteSDP(reneg); err != nil {
		t.Fatalf("HandleRemoteSDP(offer): %v", err)
	}
	select {
	case sdp := <-localSDP:
		if sdp.Type != webrtc.SDPTypeAnswer {
			t.Fatalf("local SDP type = %s, want answer", sdp.Type)
		}
		if err := sfu.SetRemoteDescription(sdp); err != nil {
			t.Fatalf("sfu set remote reneg answer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting auto answer")
	}

	// 下行持续发真实 Opus 正弦包（绑定前发包失败属正常，重试）。
	pkts := encodeSinePackets(t, 15)
	stopSend := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		var seq uint16
		var ts uint32
		for i := 0; ; i++ {
			seq++
			ts += 960
			_ = downTrack.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 0x5E0B},
				Payload: pkts[i%len(pkts)],
			})
			select {
			case <-stopSend:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	defer func() { close(stopSend); <-sent }()

	// 等 HandleTrack 登记 peer-B 并解码出 PCM。
	var in *PeerInput
	for deadline = time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		in = m.Peer("peer-B")
		if in != nil && in.WrittenSamples.Load() >= 3*OpusFrameSize {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if in == nil {
		t.Fatal("HandleTrack did not register mixer input for peer-B")
	}
	if got := in.WrittenSamples.Load(); got < 3*OpusFrameSize {
		t.Fatalf("WrittenSamples = %d, want >= %d", got, 3*OpusFrameSize)
	}
	if ids := rm.PeerIDs(); len(ids) != 1 || ids[0] != "peer-B" {
		t.Fatalf("PeerIDs = %v, want [peer-B]", ids)
	}

	// 混音输入为非静音正弦（SPSC：解码侧写、测试侧读）。
	buf := make([]int16, 4*OpusFrameSize)
	n := in.ring.Read(buf)
	if n == 0 {
		t.Fatal("mixer input ring empty after decode")
	}
	if r := rmsInt16(buf[:n]); r < 500 {
		t.Fatalf("remote decode RMS %.0f too low", r)
	}

	// peer 离开 → 链路与混音输入全部销毁（stop 会等待 goroutine 退出，
	// 不返回即测试超时）。
	rm.RemovePeer("peer-B")
	if ids := rm.PeerIDs(); len(ids) != 0 {
		t.Fatalf("PeerIDs after remove = %v, want empty", ids)
	}
	if m.Peer("peer-B") != nil {
		t.Fatal("mixer input not destroyed on remove")
	}
}

// BenchmarkRemoteDecodeFrame 远端解码热路径（push → pop → 解码 → 写混音环，
// 随后排空模拟混音 tick 消费）：必须 0 allocs/op。
// TestRemoteManagerOnPeerChange 链路增删通知在锁外触发（#45 ducking 重算钩子）；
// 无实际变化（未知 peer）不触发。
func TestRemoteManagerOnPeerChange(t *testing.T) {
	m := NewMixer(func([]int16) int { return 0 })
	rm := NewRemoteManager(m, nil)
	calls := 0
	rm.OnPeerChange = func() { calls++ }

	rm.RemovePeer("nope")
	if calls != 0 {
		t.Fatalf("未知 peer 不应触发回调，实际 %d 次", calls)
	}
	rm.RemoveAll()
	if calls != 1 {
		t.Fatalf("RemoveAll 应触发一次回调，实际 %d 次", calls)
	}
}

func BenchmarkRemoteDecodeFrame(b *testing.B) {
	m := NewMixer(func([]int16) int { return FrameSize })
	rp := newTestRemotePeer(b, "a", m)
	pkts := encodeSinePackets(b, 3)

	// 预填 3 包并消费完，进入稳态。
	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(i), pkts[i], remoteArrival(i))
	}
	drain := make([]int16, OpusFrameSize)
	for i := 0; i < 3; i++ {
		rp.decodeOnce()
		rp.input.ring.Read(drain)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rp.jb.Push(uint16(3+i), pkts[i%3], remoteArrival(3+i))
		rp.decodeOnce()
		rp.input.ring.Read(drain)
	}
}

// BenchmarkRemoteDecodePLCFrame 丢包路径：只推偶数 seq，每包弹两拍
// （空洞先探测后继包 LBRR，无 FEC/DRED 时回落 PLC + 正常帧），验证丢帧分支
// （含 PeekNext、LBRR 探测与 DRED 解析）同样零分配。
func BenchmarkRemoteDecodePLCFrame(b *testing.B) {
	m := NewMixer(func([]int16) int { return FrameSize })
	rp := newTestRemotePeer(b, "a", m)
	pkts := encodeSinePackets(b, 3)

	// 预填 0,2,4 并消费 5 拍（0 正常 1 PLC 2 正常 3 PLC 4 正常）。
	for i := 0; i < 3; i++ {
		rp.jb.Push(uint16(2*i), pkts[i], remoteArrival(2*i))
	}
	drain := make([]int16, OpusFrameSize)
	for i := 0; i < 5; i++ {
		rp.decodeOnce()
		rp.input.ring.Read(drain)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rp.jb.Push(uint16(6+2*i), pkts[i%3], remoteArrival(6+2*i))
		rp.decodeOnce() // 奇数 seq 空洞：PLC
		rp.decodeOnce() // 偶数 seq：正常
		rp.input.ring.Read(drain)
		rp.input.ring.Read(drain)
	}
}
