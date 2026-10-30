//go:build windows || linux

package codec

import (
	"math"
	"testing"
)

const (
	testSampleRate = 48000
	testChannels   = 1
	testFrameSize  = 960 // 20ms
)

func rms(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var acc float64
	for _, s := range pcm {
		acc += float64(s) * float64(s)
	}
	return math.Sqrt(acc / float64(len(pcm)))
}

// fillSine 生成 440Hz、幅度 amp 的正弦波。
func fillSine(pcm []int16, amp float64, startSample int) {
	for i := range pcm {
		t := float64(startSample+i) / testSampleRate
		pcm[i] = int16(amp * math.Sin(2*math.Pi*440*t))
	}
}

func newTestEncoder(t *testing.T) *Encoder {
	t.Helper()
	enc, err := NewEncoder(testSampleRate, testChannels, AppVoIP)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	t.Cleanup(func() { enc.Close() })
	for name, fn := range map[string]func() error{
		"SetBitrate(32000)":     func() error { return enc.SetBitrate(32000) },
		"SetInBandFEC(true)":    func() error { return enc.SetInBandFEC(true) },
		"SetDTX(true)":          func() error { return enc.SetDTX(true) },
		"SetPacketLossPerc(10)": func() error { return enc.SetPacketLossPerc(10) },
	} {
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return enc
}

func newTestDecoder(t *testing.T) *Decoder {
	t.Helper()
	dec, err := NewDecoder(testSampleRate, testChannels)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	t.Cleanup(func() { dec.Close() })
	return dec
}

// TestRoundTrip 正弦波 100 帧编解码往返：信号不丢失、码率符合预期。
func TestRoundTrip(t *testing.T) {
	enc := newTestEncoder(t)
	dec := newTestDecoder(t)

	pcm := make([]int16, testFrameSize)
	out := make([]int16, testFrameSize)
	buf := make([]byte, 4000)

	const frames = 100
	var totalBytes int
	var inRMS, outRMS float64

	for f := 0; f < frames; f++ {
		fillSine(pcm, 12000, f*testFrameSize)
		inRMS += rms(pcm)

		n, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("frame %d Encode: %v", f, err)
		}
		if n <= 0 {
			t.Fatalf("frame %d: encoded %d bytes", f, n)
		}
		totalBytes += n

		m, err := dec.Decode(buf[:n], out, false)
		if err != nil {
			t.Fatalf("frame %d Decode: %v", f, err)
		}
		if m != testFrameSize {
			t.Fatalf("frame %d: decoded %d samples, want %d", f, m, testFrameSize)
		}
		outRMS += rms(out)
	}

	inRMS /= frames
	outRMS /= frames
	avgBytes := float64(totalBytes) / frames
	kbps := avgBytes * 8 * (testSampleRate / testFrameSize) / 1000 // bytes/帧 → kbps
	t.Logf("avg frame: %.1f bytes (%.1f kbps), in RMS %.0f, out RMS %.0f", avgBytes, kbps, inRMS, outRMS)

	if outRMS < inRMS*0.5 {
		t.Errorf("decoded signal too weak: out RMS %.0f < 50%% of in RMS %.0f", outRMS, inRMS)
	}
	if kbps < 20 || kbps > 45 {
		t.Errorf("bitrate %.1f kbps outside expected window around 32kbps", kbps)
	}
}

// TestPLC 丢包隐藏：空包解码应产出与帧长一致的非静音隐藏帧，且后续包可正常解码。
func TestPLC(t *testing.T) {
	enc := newTestEncoder(t)
	dec := newTestDecoder(t)

	pcm := make([]int16, testFrameSize)
	out := make([]int16, testFrameSize)
	buf := make([]byte, 4000)

	pkts := make([][]byte, 10)
	for i := range pkts {
		fillSine(pcm, 12000, i*testFrameSize)
		n, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("frame %d Encode: %v", i, err)
		}
		pkts[i] = append([]byte(nil), buf[:n]...)
	}

	// 前 8 帧正常解码。
	for i := 0; i < 8; i++ {
		if _, err := dec.Decode(pkts[i], out, false); err != nil {
			t.Fatalf("frame %d Decode: %v", i, err)
		}
	}

	// 第 9 帧「丢失」：PLC。
	m, err := dec.Decode(nil, out, false)
	if err != nil {
		t.Fatalf("PLC Decode: %v", err)
	}
	if m != testFrameSize {
		t.Fatalf("PLC produced %d samples, want %d", m, testFrameSize)
	}
	if r := rms(out); r < 100 {
		t.Errorf("PLC output near-silent (RMS %.0f), concealment failed", r)
	}

	// 第 10 帧仍可正常解码（PLC 后解码器状态连续）。
	m, err = dec.Decode(pkts[9], out, false)
	if err != nil {
		t.Fatalf("post-PLC Decode: %v", err)
	}
	if m != testFrameSize {
		t.Fatalf("post-PLC decoded %d samples, want %d", m, testFrameSize)
	}
}

// TestFEC 带内 FEC：用下一包恢复上一帧，长度正确且非静音。
func TestFEC(t *testing.T) {
	enc := newTestEncoder(t)
	dec := newTestDecoder(t)

	pcm := make([]int16, testFrameSize)
	out := make([]int16, testFrameSize)
	buf := make([]byte, 4000)

	pkts := make([][]byte, 4)
	for i := range pkts {
		fillSine(pcm, 12000, i*testFrameSize)
		n, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("frame %d Encode: %v", i, err)
		}
		pkts[i] = append([]byte(nil), buf[:n]...)
	}

	// 正常解第 0 帧。
	if _, err := dec.Decode(pkts[0], out, false); err != nil {
		t.Fatalf("frame 0 Decode: %v", err)
	}
	// 第 1 帧「丢失」：用第 2 个包的 FEC 恢复。
	m, err := dec.Decode(pkts[2], out, true)
	if err != nil {
		t.Fatalf("FEC Decode: %v", err)
	}
	if m != testFrameSize {
		t.Fatalf("FEC recovered %d samples, want %d", m, testFrameSize)
	}
	if r := rms(out); r < 100 {
		t.Errorf("FEC output near-silent (RMS %.0f)", r)
	}
}

// TestDREDRecoversLostFrame 深度冗余（DRED）：用后继包里的冗余重建紧邻其前
// 的丢帧。DRED 只有在预期丢包率足够高时才产出，故编码端 PLP=20。
func TestDREDRecoversLostFrame(t *testing.T) {
	enc, err := NewEncoder(testSampleRate, testChannels, AppVoIP)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	defer enc.Close()
	if err := enc.SetBitrate(32000); err != nil {
		t.Fatalf("SetBitrate: %v", err)
	}
	if err := enc.SetInBandFEC(false); err != nil {
		t.Fatalf("SetInBandFEC: %v", err)
	}
	if err := enc.SetDRED(20); err != nil {
		t.Fatalf("SetDRED: %v", err)
	}
	if err := enc.SetPacketLossPerc(20); err != nil {
		t.Fatalf("SetPacketLossPerc: %v", err)
	}
	if d, err := enc.DREDDuration(); err != nil || d != 20 {
		t.Fatalf("DREDDuration = %d, %v; want 20", d, err)
	}
	if p, err := enc.PacketLossPerc(); err != nil || p != 20 {
		t.Fatalf("PacketLossPerc = %d, %v; want 20", p, err)
	}

	pcm := make([]int16, testFrameSize)
	out := make([]int16, testFrameSize)
	buf := make([]byte, 4000)
	pkts := make([][]byte, 6)
	for i := range pkts {
		fillSine(pcm, 8000+1000*float64(i), i*testFrameSize)
		n, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("frame %d Encode: %v", i, err)
		}
		pkts[i] = append([]byte(nil), buf[:n]...)
	}

	dec := newTestDecoder(t)
	dred, err := NewDREDDecoder()
	if err != nil {
		t.Fatalf("NewDREDDecoder: %v", err)
	}
	defer dred.Close()

	// 丢帧前必须正常解码过若干包，DRED 解码依赖解码器的 warm 状态。
	for i := 0; i < 3; i++ {
		if _, err := dec.Decode(pkts[i], out, false); err != nil {
			t.Fatalf("warm decode %d: %v", i, err)
		}
	}

	// 第 3 帧丢失：用后继包 4 的 DRED 恢复。FEC 已关，包内不应有 LBRR
	// （否则 opus_decode(fec=true) 会先把它当 FEC 用掉）。
	if HasLBRR(pkts[4]) {
		t.Fatal("FEC 已关闭，后继包不应携带 LBRR")
	}
	n, ok, err := dred.Recover(dec, pkts[4], out, testFrameSize, testSampleRate)
	if err != nil {
		t.Fatalf("DRED Recover: %v", err)
	}
	if !ok {
		t.Fatal("后继包不含 DRED（PLP 不足或库未启用 DRED）")
	}
	if n != testFrameSize {
		t.Fatalf("DRED recovered %d samples, want %d", n, testFrameSize)
	}
	if r := rms(out); r < 1000 {
		t.Fatalf("DRED 输出接近静音（RMS %.0f），解码器状态未热或冗余解析失败", r)
	}

	// DRED 恢复后解码器仍能正常解码后继包。
	if _, err := dec.Decode(pkts[4], out, false); err != nil {
		t.Fatalf("post-DRED Decode: %v", err)
	}

	// 关闭 DRED 后 ctl 归零，PLP 恢复用户值。
	if err := enc.SetDRED(0); err != nil {
		t.Fatalf("SetDRED(0): %v", err)
	}
	if d, err := enc.DREDDuration(); err != nil || d != 0 {
		t.Fatalf("DREDDuration after disable = %d, %v; want 0", d, err)
	}
	if err := enc.SetPacketLossPerc(10); err != nil {
		t.Fatalf("SetPacketLossPerc(10): %v", err)
	}
	if p, err := enc.PacketLossPerc(); err != nil || p != 10 {
		t.Fatalf("PacketLossPerc = %d, %v; want 10", p, err)
	}
}

// TestSettersAndVersion 杂项 setter 与版本串。
func TestSettersAndVersion(t *testing.T) {
	enc := newTestEncoder(t)
	if err := enc.SetVBR(true); err != nil {
		t.Fatalf("SetVBR: %v", err)
	}
	if v := Version(); v == "" {
		t.Fatal("Version() empty")
	} else {
		t.Logf("libopus version: %s", v)
	}
	if _, err := enc.Encode(nil, make([]byte, 100)); err != ErrEmptyBuffer {
		t.Fatalf("Encode(nil): got %v, want ErrEmptyBuffer", err)
	}
}

// BenchmarkEncode 编码热路径：必须 0 allocs/op。
func BenchmarkEncode(b *testing.B) {
	enc, err := NewEncoder(testSampleRate, testChannels, AppVoIP)
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	enc.SetBitrate(32000)

	pcm := make([]int16, testFrameSize)
	fillSine(pcm, 12000, 0)
	buf := make([]byte, 4000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode(pcm, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecode 解码热路径：必须 0 allocs/op。
func BenchmarkDecode(b *testing.B) {
	enc, err := NewEncoder(testSampleRate, testChannels, AppVoIP)
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	enc.SetBitrate(32000)
	dec, err := NewDecoder(testSampleRate, testChannels)
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Close()

	pcm := make([]int16, testFrameSize)
	fillSine(pcm, 12000, 0)
	buf := make([]byte, 4000)
	n, err := enc.Encode(pcm, buf)
	if err != nil {
		b.Fatal(err)
	}
	pkt := buf[:n]
	out := make([]int16, testFrameSize)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := dec.Decode(pkt, out, false); err != nil {
			b.Fatal(err)
		}
	}
}
