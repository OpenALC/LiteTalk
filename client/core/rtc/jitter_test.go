package rtc

import (
	"testing"
	"time"
)

var jitterBase = time.UnixMilli(1_700_000_000_000)

// pushAt 以固定 20ms 到达间隔推入 seq（payload 内容为 seq 低字节，便于校验）。
func pushAt(b *JitterBuffer, seq uint16, at time.Time) {
	b.Push(seq, []byte{byte(seq), byte(seq >> 8), 0xAB}, at)
}

// pushInOrder 从 start 起推 n 个连续包，间隔 20ms。
func pushInOrder(b *JitterBuffer, start uint16, n int) {
	for i := 0; i < n; i++ {
		pushAt(b, start+uint16(i), jitterBase.Add(time.Duration(start+uint16(i))*JitterFrameMS*time.Millisecond))
	}
}

func TestJitterPrefillAndInOrderPlayout(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	// 预填未满（目标 3 包）：Pop 不出帧。
	pushInOrder(jb, 100, 2)
	if _, _, ok := jb.Pop(out); ok {
		t.Fatal("Pop before prefill should not be ok")
	}

	// 第 3 包到达 → 开始按序弹出。
	pushAt(jb, 102, jitterBase.Add(102*JitterFrameMS*time.Millisecond))
	for want := uint16(100); want <= 102; want++ {
		n, lost, ok := jb.Pop(out)
		if !ok || lost {
			t.Fatalf("Pop seq %d: lost=%v ok=%v, want normal frame", want, lost, ok)
		}
		if n != 3 || out[0] != byte(want) || out[1] != byte(want>>8) {
			t.Fatalf("Pop payload %v, want seq %d marker", out[:n], want)
		}
	}
	st := jb.Stats()
	if !st.Started {
		t.Fatal("should be started")
	}
	if st.Buffered != 0 {
		t.Fatalf("Buffered = %d, want 0", st.Buffered)
	}
}

func TestJitterReorderWithinWindow(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	// 乱序推入 100,102,101,104,103：全部入槽后应能按序弹出。
	for _, seq := range []uint16{100, 102, 101, 104, 103} {
		pushAt(jb, seq, jitterBase)
	}
	for want := uint16(100); want <= 104; want++ {
		_, lost, ok := jb.Pop(out)
		if !ok || lost {
			t.Fatalf("Pop seq %d: lost=%v ok=%v", want, lost, ok)
		}
		if out[0] != byte(want) {
			t.Fatalf("got seq marker %d, want %d", out[0], want)
		}
	}
	if st := jb.Stats(); st.Lost != 0 {
		t.Fatalf("Lost = %d, want 0 (reorder recovered)", st.Lost)
	}
}

func TestJitterLossMarkingAndLateDrop(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	// 预填 100,101,102（缺 103 前先消费）。
	pushInOrder(jb, 100, 3)
	jb.Pop(out) // 100
	jb.Pop(out) // 101
	jb.Pop(out) // 102

	// 103 丢失，104 先到：Pop 103 时应判定 lost（headSeq=104 >= nextPop=103）。
	pushAt(jb, 104, jitterBase)
	if _, lost, ok := jb.Pop(out); !ok || !lost {
		t.Fatalf("Pop for missing 103: lost=%v ok=%v, want lost frame", lost, ok)
	}

	// 103 迟到：应被丢弃（ext < nextPop）。
	pushAt(jb, 103, jitterBase)
	if st := jb.Stats(); st.DroppedLate != 1 {
		t.Fatalf("DroppedLate = %d, want 1", st.DroppedLate)
	}

	// 104 正常弹出。
	if n, lost, ok := jb.Pop(out); !ok || lost || n == 0 {
		t.Fatalf("Pop 104: n=%d lost=%v ok=%v, want normal", n, lost, ok)
	}
	if st := jb.Stats(); st.Lost != 1 {
		t.Fatalf("Lost = %d, want 1", st.Lost)
	}
}

func TestJitterDuplicateDrop(t *testing.T) {
	jb := NewJitterBuffer()
	pushInOrder(jb, 100, 3)
	pushAt(jb, 101, jitterBase) // 重复
	pushAt(jb, 101, jitterBase) // 再重复
	if st := jb.Stats(); st.Duplicated != 2 {
		t.Fatalf("Duplicated = %d, want 2", st.Duplicated)
	}
}

func TestJitterStarvationReturnsToPrefill(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	pushInOrder(jb, 100, 3)
	jb.Pop(out) // 100
	jb.Pop(out) // 101
	jb.Pop(out) // 102 → 此后断流

	// 断流期：出 PLC 帧（lost=true），最多 2*target 拍后回预填态。
	plcs := 0
	for i := 0; i < 20; i++ {
		_, lost, ok := jb.Pop(out)
		if !ok {
			break
		}
		if !lost {
			t.Fatal("starvation pop should be lost (PLC)")
		}
		plcs++
	}
	if plcs == 0 || plcs > 2*6+1 {
		t.Fatalf("PLC frames during starvation = %d, want in (0, 13]", plcs)
	}
	if st := jb.Stats(); st.Started {
		t.Fatal("should return to prefill after long starvation")
	}

	// 流恢复：重新预填后按序播放。
	pushInOrder(jb, 200, 3)
	for want := uint16(200); want <= 202; want++ {
		_, lost, ok := jb.Pop(out)
		if !ok || lost {
			t.Fatalf("Pop seq %d after recovery: lost=%v ok=%v", want, lost, ok)
		}
	}
}

// TestJitterDTXHeartbeatNoHole 静音期每 20ms 一个 1~2 字节 DTX 心跳包（发送端
// 不再丢弃）：jitter 视其为正常到达帧，不判空洞、不重回预填。
func TestJitterDTXHeartbeatNoHole(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	pushInOrder(jb, 100, 3)
	for want := uint16(100); want <= 102; want++ {
		if _, lost, ok := jb.Pop(out); !ok || lost {
			t.Fatalf("prefill pop %d: lost=%v ok=%v", want, lost, ok)
		}
	}

	// DTX 心跳：1 字节载荷，按 20ms 节拍与 Pop 交错。
	for i := 0; i < 30; i++ {
		seq := uint16(103 + i)
		jb.Push(seq, []byte{0xFF}, jitterBase.Add(time.Duration(seq)*JitterFrameMS*time.Millisecond))
		n, lost, ok := jb.Pop(out)
		if !ok {
			t.Fatalf("tick %d: jitter re-entered prefill on DTX heartbeat", i)
		}
		if lost {
			t.Fatalf("tick %d: DTX heartbeat judged as loss", i)
		}
		if n != 1 {
			t.Fatalf("tick %d: payload = %d bytes, want 1", i, n)
		}
	}

	st := jb.Stats()
	if st.Lost != 0 {
		t.Fatalf("Lost = %d, want 0", st.Lost)
	}
	if !st.Started {
		t.Fatal("should stay started through DTX silence")
	}
	if st.Resets != 0 {
		t.Fatalf("Resets = %d, want 0", st.Resets)
	}
}

// TestJitterPeekNextDoesNotConsume 窥视后继包不推进消费位置、不改缓冲计数；
// 随后 Pop 仍能拿到同一包（FEC 恢复依赖此语义）。
func TestJitterPeekNextDoesNotConsume(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	pushInOrder(jb, 100, 4)
	jb.Pop(out) // 100
	jb.Pop(out) // 101

	before := jb.Stats()
	n, ok := jb.PeekNext(out)
	if !ok || n != 3 || out[0] != 102 {
		t.Fatalf("PeekNext = (%d, %v), payload %v; want 102", n, ok, out[:n])
	}
	if n2, ok2 := jb.PeekNext(out); !ok2 || n2 != n {
		t.Fatalf("second PeekNext = (%d, %v), want (%d, true)", n2, ok2, n)
	}
	after := jb.Stats()
	if after.Buffered != before.Buffered || after.Received != before.Received {
		t.Fatalf("PeekNext mutated stats: before=%+v after=%+v", before, after)
	}

	if n, lost, ok := jb.Pop(out); !ok || lost || n != 3 || out[0] != 102 {
		t.Fatalf("Pop after PeekNext = (%d, lost=%v, ok=%v), want 102", n, lost, ok)
	}

	// 无后继（连续丢包）时返回 ok=false。
	jb.Pop(out) // 103
	if _, ok := jb.PeekNext(out); ok {
		t.Fatal("PeekNext on empty successor should be false")
	}
}

func TestJitterLargeJumpReset(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	pushInOrder(jb, 100, 3)
	jb.Pop(out)
	jb.Pop(out)

	// 前向跳变 2000 包（> jitterResetJump）：流重启，重定基预填。
	pushInOrder(jb, 2100, 3)
	if st := jb.Stats(); st.Resets != 1 {
		t.Fatalf("Resets = %d, want 1", st.Resets)
	}
	for want := uint16(2100); want <= 2102; want++ {
		_, lost, ok := jb.Pop(out)
		if !ok || lost {
			t.Fatalf("Pop seq %d after jump: lost=%v ok=%v", want, lost, ok)
		}
	}
}

func TestJitterSeqWraparound(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	// 65534, 65535, 0, 1 连续流（跨回绕点）。
	pushAt(jb, 65534, jitterBase)
	pushAt(jb, 65535, jitterBase.Add(JitterFrameMS*time.Millisecond))
	pushAt(jb, 0, jitterBase.Add(2*JitterFrameMS*time.Millisecond))
	pushAt(jb, 1, jitterBase.Add(3*JitterFrameMS*time.Millisecond))
	for i, want := range []byte{254, 255, 0, 1} {
		n, lost, ok := jb.Pop(out)
		if !ok || lost {
			t.Fatalf("Pop wrap %d: lost=%v ok=%v", i, lost, ok)
		}
		if out[0] != want {
			t.Fatalf("wrap pop %d: marker %d, want %d", i, out[0], want)
		}
		_ = n
	}
}

func TestJitterAdaptiveTarget(t *testing.T) {
	jb := NewJitterBuffer()

	// 到达间隔交替 ±40ms 抖动：transit 差 ≈80ms，jitter 收敛后目标应加深。
	at := jitterBase
	for i := uint16(0); i < 200; i++ {
		pushAt(jb, 1000+i, at)
		at = at.Add(JitterFrameMS * time.Millisecond)
		if i%2 == 0 {
			at = at.Add(40 * time.Millisecond) // 晚到 40ms
		} else {
			at = at.Add(-40 * time.Millisecond) // 早回 40ms
		}
	}
	st := jb.Stats()
	minTarget := jitterMinMS / JitterFrameMS
	if st.TargetPackets <= minTarget {
		t.Fatalf("TargetPackets = %d, want > %d under 80ms jitter (jitterMs=%.1f)",
			st.TargetPackets, minTarget, st.JitterMs)
	}
	t.Logf("jitterMs=%.1f target=%d packets (%dms)", st.JitterMs, st.TargetPackets, st.TargetPackets*JitterFrameMS)
}

func TestJitterSteadyPlayout(t *testing.T) {
	// 模拟稳态流：预填 6 包后每拍推一包弹一包（在槽深度 ≤ 槽环容量），
	// 50 拍弹出序列应完全一致且无丢包。
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)
	pushInOrder(jb, 5000, 6)
	for i := 0; i < 50; i++ {
		n, lost, ok := jb.Pop(out)
		if !ok || lost {
			t.Fatalf("steady pop %d: lost=%v ok=%v", i, lost, ok)
		}
		seq := uint16(out[0]) | uint16(out[1])<<8
		if want := uint16(5000 + i); seq != want {
			t.Fatalf("frame %d: seq %d, want %d", i, seq, want)
		}
		_ = n
		if next := 5006 + i; next < 5050 {
			pushAt(jb, uint16(next), jitterBase.Add(time.Duration(next)*JitterFrameMS*time.Millisecond))
		}
	}
}

// TestJitterFrameMS10 10ms 帧长（#46）：transit 按 10ms 计算（等间隔到达不产生
// 虚假抖动），预填目标按 60ms 换算成 6 包；非法帧长拒绝。
func TestJitterFrameMS10(t *testing.T) {
	jb := NewJitterBuffer()
	if err := jb.SetFrameMS(10); err != nil {
		t.Fatalf("SetFrameMS(10): %v", err)
	}
	if err := jb.SetFrameMS(15); err == nil {
		t.Fatal("非法帧长应报错")
	}
	for i := 0; i < 6; i++ {
		seq := uint16(200 + i)
		at := jitterBase.Add(time.Duration(i) * 10 * time.Millisecond)
		jb.Push(seq, []byte{byte(seq)}, at)
	}
	st := jb.Stats()
	if st.TargetPackets != 6 {
		t.Fatalf("10ms 目标深度 = %d 包, want 6（60ms）", st.TargetPackets)
	}
	if st.JitterMs > 1 {
		t.Fatalf("等间隔 10ms 到达的抖动估计 = %.2fms，不应出现虚假抖动", st.JitterMs)
	}
	out := make([]byte, JitterMaxPayload)
	for i := 0; i < 6; i++ {
		if n, lost, ok := jb.Pop(out); !ok || lost || n != 1 {
			t.Fatalf("第 %d 拍 Pop = n%d lost=%v ok=%v，预填应已满", i, n, lost, ok)
		}
	}

	// 切回 20ms：目标深度回落 3 包，transit 基准重置（不把切换当抖动尖峰）。
	if err := jb.SetFrameMS(20); err != nil {
		t.Fatal(err)
	}
	if got := jb.Stats().TargetPackets; got != 3 {
		t.Fatalf("切回 20ms 后目标深度 = %d, want 3", got)
	}
}

// TestJitterFrameSwitchFlush 帧长热切换冲刷旧帧（#117）：切换后槽内不得残留旧帧，
// 切换前已在途的旧包（含乱序晚到）一律丢弃，首帧延迟等于新帧长的预填深度。
func TestJitterFrameSwitchFlush(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)

	// 20ms 稳态：预填 3 包 → 弹 3 包，槽内清空。
	pushInOrder(jb, 100, 3)
	for i := 0; i < 3; i++ {
		if _, lost, ok := jb.Pop(out); !ok || lost {
			t.Fatalf("20ms pop %d: lost=%v ok=%v", i, lost, ok)
		}
	}
	if st := jb.Stats(); st.Buffered != 0 || !st.Started {
		t.Fatalf("切换前状态 = %+v, want started 且槽空", st)
	}

	// 切 10ms：槽内已无帧，但要回到预填态（目标 6 包 = 60ms）。
	if err := jb.SetFrameMS(10); err != nil {
		t.Fatalf("SetFrameMS(10): %v", err)
	}
	if st := jb.Stats(); st.Buffered != 0 || st.Started {
		t.Fatalf("切换后状态 = %+v, want Buffered=0 且未开始", st)
	}
	if _, _, ok := jb.Pop(out); ok {
		t.Fatal("切换后未重新预填就出帧了")
	}

	// 切换前已到达过、但乱序晚到的旧 20ms 包（seq 101 ≤ 切换时刻最大序号 102）：
	// 若被收进槽会以旧帧长参与预填，必须丢弃。
	jb.Push(101, []byte{101, 0, 0xAB}, jitterBase.Add(101*time.Millisecond))
	if st := jb.Stats(); st.Buffered != 0 {
		t.Fatalf("旧帧长包被收进槽（Buffered=%d）", st.Buffered)
	}

	// 新帧长 10ms、100ms 间隔的 6 包填满预填目标；首帧延迟 = 6×10ms。
	for i := 0; i < 6; i++ {
		seq := uint16(104 + i)
		jb.Push(seq, []byte{byte(seq), byte(seq >> 8), 0xAB}, jitterBase.Add(time.Duration(i)*10*time.Millisecond))
	}
	if st := jb.Stats(); st.TargetPackets != 6 || st.Buffered != 6 {
		t.Fatalf("10ms 预填状态 = %+v, want target=6 buffered=6", st)
	}
	n, lost, ok := jb.Pop(out)
	if !ok || lost || n != 3 || out[0] != 104 {
		t.Fatalf("切换后首帧 = n%d lost=%v ok=%v payload=%v, want seq 104", n, lost, ok, out[:n])
	}
	for i := 1; i < 6; i++ {
		if _, lost, ok := jb.Pop(out); !ok || lost || out[0] != byte(104+i) {
			t.Fatalf("切换后第 %d 帧 = seq %d lost=%v ok=%v, want %d", i, out[0], lost, ok, 104+i)
		}
	}

	// 切回 20ms：同样冲刷并回到 3 包预填，旧 10ms 帧不再参与。
	if err := jb.SetFrameMS(20); err != nil {
		t.Fatalf("SetFrameMS(20): %v", err)
	}
	if _, _, ok := jb.Pop(out); ok {
		t.Fatal("切回 20ms 后未重新预填就出帧了")
	}
	for i := 0; i < 3; i++ {
		seq := uint16(200 + i)
		jb.Push(seq, []byte{byte(seq), byte(seq >> 8), 0xAB}, jitterBase.Add(time.Duration(i)*20*time.Millisecond))
	}
	n, lost, ok = jb.Pop(out)
	if !ok || lost || out[0] != 200 {
		t.Fatalf("切回 20ms 首帧 = n%d lost=%v ok=%v payload=%v, want seq 200", n, lost, ok, out[:n])
	}
}

// TestJitterFrameSwitchDropsBuffered 切换时槽内有未消费的旧帧（#117）：必须被
// 清掉，否则旧帧长采样会持续垫高播放延迟。
func TestJitterFrameSwitchDropsBuffered(t *testing.T) {
	jb := NewJitterBuffer()
	out := make([]byte, JitterMaxPayload)
	pushInOrder(jb, 300, 8) // 8 包 → 预填 3 包后开始，余 5 包留在槽里
	if _, _, ok := jb.Pop(out); !ok {
		t.Fatal("预填应已完成")
	}
	if st := jb.Stats(); st.Buffered == 0 {
		t.Fatal("测试前置：槽内应还有未消费的旧帧")
	}
	if err := jb.SetFrameMS(10); err != nil {
		t.Fatal(err)
	}
	if st := jb.Stats(); st.Buffered != 0 || st.Started {
		t.Fatalf("切换后残留 = %+v, want Buffered=0 未开始", st)
	}
	// 槽位被清空后，旧 seq 若在槽内会随即被弹出；确认 5 拍内没有再吐出旧帧。
	for i := 0; i < 5; i++ {
		if _, _, ok := jb.Pop(out); ok {
			t.Fatalf("第 %d 拍仍在弹旧帧（payload=%v）", i, out[:3])
		}
	}
}

// BenchmarkJitterPushPop 稳态推拉热点：必须 0 allocs/op。
func BenchmarkJitterPushPop(b *testing.B) {
	jb := NewJitterBuffer()
	payload := make([]byte, 100) // 典型 20ms Opus 包量级
	out := make([]byte, JitterMaxPayload)
	// 预填。
	for i := uint16(0); i < 6; i++ {
		jb.Push(i, payload, jitterBase)
	}
	at := jitterBase
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		at = at.Add(JitterFrameMS * time.Millisecond)
		jb.Push(uint16(6+i), payload, at)
		jb.Pop(out)
	}
}
