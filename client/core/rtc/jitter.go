package rtc

import (
	"math"
	"sync"
	"time"
)

// jitter buffer 常量。
const (
	// JitterFrameMS 每包媒体时长（Opus 20ms 帧）。
	JitterFrameMS = 20
	// 环形槽位数：32 槽 × 20ms = 640ms 容量。
	jitterSlots = 32
	// JitterMaxPayload 单槽载荷缓冲上限（20ms Opus 远小于此）。
	JitterMaxPayload = 1500
	// 自适应窗口：60ms（3 包）～120ms（6 包）。
	jitterMinTarget = 3
	jitterMaxTarget = 6
	// 前向跳变超过该值视为流重启，清空重定基。
	jitterResetJump = 1000
)

// JitterStats 抖动缓冲统计快照。
type JitterStats struct {
	Received      uint64  // 收到的包数（含重复/迟到）
	Duplicated    uint64  // 重复丢弃
	DroppedLate   uint64  // 迟到丢弃（已被消费或超出窗口）
	Lost          uint64  // 判定丢失（seq 空洞，交 PLC）
	Resets        uint64  // 大跳变重定基次数
	Truncated     uint64  // 载荷超槽位缓冲被截断
	JitterMs      float64 // 到达间隔抖动估计（RFC3550 风格，ms）
	TargetPackets int     // 当前自适应目标深度（包）
	Buffered      int     // 当前槽内在缓冲包数
	Started       bool    // 是否已过预填期
}

// JitterBuffer 是 per-peer 的 RTP 抖动缓冲：按 seq 排序、60–120ms 自适应
// 预填窗口、丢包标记（供 PLC）、重复/迟到丢弃。
//
// 线程模型：Push（网络读 goroutine）与 Pop（20ms 消费 goroutine）经互斥锁
// 并发安全；热路径全部使用预分配槽位，零堆分配。
//
// 使用节奏：消费者每 20ms 调一次 Pop；ok=false 表示预填中/断流静默（本拍
// 不出帧），lost=true 表示该帧判定丢失（调用方应 PLC）。
type JitterBuffer struct {
	mu sync.Mutex

	slots [jitterSlots]jitterSlot

	init     bool   // 是否见过首包
	headSeq  uint64 // 见过的最大扩展序号
	count    int    // 槽内在缓冲包数
	firstSeq uint64 // 预填基准（!started 时的最老包 seq）

	started bool   // 是否已过预填期
	nextPop uint64 // 下一个应弹出的扩展序号
	starved int    // 连续断流拍数

	lastTransit float64 // 上一包 transit（到达时刻 - seq*20ms，ms）
	hasTransit  bool
	jitterMs    float64 // RFC3550 风格抖动估计

	targetPackets int // 当前自适应目标深度

	// 统计（锁内更新，Stats 锁内读）
	received    uint64
	duplicated  uint64
	droppedLate uint64
	lost        uint64
	resets      uint64
	truncated   uint64
}

type jitterSlot struct {
	seq     uint64
	buf     [JitterMaxPayload]byte
	n       int
	present bool
}

// NewJitterBuffer 创建抖动缓冲（初始目标深度 60ms）。
func NewJitterBuffer() *JitterBuffer {
	return &JitterBuffer{targetPackets: jitterMinTarget}
}

// Push 推入一个 RTP 包（seq 为 RTP 序号，payload 为载荷，arrival 为到达时刻）。
// 重复、迟到、过旧的包静默丢弃并计数。payload 内容被拷贝进预分配槽位，
// 返回后调用方即可复用底层缓冲。
func (b *JitterBuffer) Push(seq uint16, payload []byte, arrival time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.received++

	ext := extendSeq(b.headSeq, b.init, seq)
	if !b.init {
		b.init = true
		b.headSeq = ext
		b.firstSeq = ext
	} else {
		switch {
		case b.started && ext < b.nextPop:
			b.droppedLate++ // 已消费过的序号（含重放/重复）
			return
		case ext < b.headSeq && b.headSeq-ext >= jitterSlots:
			b.droppedLate++ // 太旧，槽环已容纳不下
			return
		case ext > b.headSeq && ext-b.headSeq >= jitterResetJump:
			b.resetLocked(ext) // 大跳变：流重启/长时间中断恢复
		}
	}

	slot := &b.slots[ext%jitterSlots]
	if slot.present && slot.seq == ext {
		b.duplicated++
		return
	}
	n := len(payload)
	if n > JitterMaxPayload {
		n = JitterMaxPayload
		b.truncated++
	}
	copy(slot.buf[:n], payload[:n])
	if !slot.present {
		b.count++ // 覆写旧占用者（seq 差为槽数整数倍）时槽位数不变
	}
	slot.seq = ext
	slot.n = n
	slot.present = true
	if !b.started {
		if b.count == 1 || ext < b.firstSeq {
			b.firstSeq = ext // 预填基准：最老在槽包（兼容初始乱序与断流重预填）
		}
	}
	if ext > b.headSeq {
		b.headSeq = ext
	}

	b.updateJitterLocked(ext, arrival)
	b.adaptTargetLocked()
}

// Pop 弹出下一帧（每 20ms 调用一次）：
//   - ok=false：预填未满或断流后静默期，本拍不出帧；
//   - lost=true, ok=true：该帧判定丢失，out 未写入，调用方应做 PLC；
//   - lost=false, ok=true：正常帧，载荷在 out[:n]。
//
// 断流判定：期望包与更新包都未到 → 出 PLC 帧并推进；连续断流超过
// 2 倍目标深度 → 回到预填态（流恢复后重新缓冲）。
func (b *JitterBuffer) Pop(out []byte) (n int, lost bool, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.init {
		return 0, false, false
	}
	if !b.started {
		if b.count == 0 || b.headSeq-b.firstSeq+1 < uint64(b.targetPackets) {
			return 0, false, false
		}
		b.started = true
		b.nextPop = b.firstSeq
	}

	slot := &b.slots[b.nextPop%jitterSlots]
	if slot.present && slot.seq == b.nextPop {
		n = slot.n
		if len(out) < n {
			n = len(out) // 防御：调用方缓冲不足时截断（正常路径不会发生）
		}
		copy(out[:n], slot.buf[:slot.n])
		slot.present = false
		b.count--
		b.nextPop++
		b.starved = 0
		return n, false, true
	}

	if b.headSeq >= b.nextPop {
		// 空洞：更新的包已在槽内，该包判定丢失（或深度乱序，等不起了）。
		b.lost++
		b.nextPop++
		return 0, true, true
	}

	// 断流：期望包未到，也没有更新的包。
	b.starved++
	if b.starved > 2*b.targetPackets {
		// 中断过长：回到预填态。此刻 headSeq < nextPop，槽内必然无未消费包。
		b.started = false
		b.starved = 0
		b.count = 0
		for i := range b.slots {
			b.slots[i].present = false
		}
		return 0, false, false
	}
	b.lost++
	b.nextPop++
	return 0, true, true
}

// PeekNext 复制「下一个应弹出」槽位的载荷，但不消费：不推进 nextPop、
// 不改缓冲计数与断流状态。仅用于丢帧瞬间取后继包做带内 FEC 恢复
// （先 Pop 得到 lost，再 PeekNext 判断后继是否已到）；连续丢包时 ok=false。
// 锁语义与 Pop 一致，零分配。
func (b *JitterBuffer) PeekNext(out []byte) (n int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.started {
		return 0, false
	}
	slot := &b.slots[b.nextPop%jitterSlots]
	if !slot.present || slot.seq != b.nextPop {
		return 0, false
	}
	n = slot.n
	if len(out) < n {
		n = len(out) // 防御：与 Pop 相同的截断策略
	}
	copy(out[:n], slot.buf[:n])
	return n, true
}

// Stats 返回统计快照。
func (b *JitterBuffer) Stats() JitterStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return JitterStats{
		Received:      b.received,
		Duplicated:    b.duplicated,
		DroppedLate:   b.droppedLate,
		Lost:          b.lost,
		Resets:        b.resets,
		Truncated:     b.truncated,
		JitterMs:      b.jitterMs,
		TargetPackets: b.targetPackets,
		Buffered:      b.count,
		Started:       b.started,
	}
}

// resetLocked 大跳变重定基：清空全部槽位，以 ext 为新基准重新预填。
func (b *JitterBuffer) resetLocked(ext uint64) {
	for i := range b.slots {
		b.slots[i].present = false
	}
	b.count = 0
	b.started = false
	b.starved = 0
	b.headSeq = ext
	b.firstSeq = ext
	b.hasTransit = false
	b.resets++
}

// updateJitterLocked 按 RFC3550 风格更新到达抖动估计：
// transit = 到达时刻 - seq×20ms；J += (|Δtransit| - J)/16。
func (b *JitterBuffer) updateJitterLocked(ext uint64, arrival time.Time) {
	transit := float64(arrival.UnixMilli()) - float64(ext)*JitterFrameMS
	if b.hasTransit {
		d := math.Abs(transit - b.lastTransit)
		b.jitterMs += (d - b.jitterMs) / 16
	}
	b.lastTransit = transit
	b.hasTransit = true
}

// adaptTargetLocked 按抖动估计调整目标深度：基础 60ms，抖动每 ~20ms 加深一包，
// 封顶 120ms。
func (b *JitterBuffer) adaptTargetLocked() {
	t := jitterMinTarget + int(b.jitterMs/JitterFrameMS+0.5)
	if t < jitterMinTarget {
		t = jitterMinTarget
	}
	if t > jitterMaxTarget {
		t = jitterMaxTarget
	}
	b.targetPackets = t
}

// extendSeq 把 16bit RTP 序号扩展为 64bit 单调序号：取与当前 head 最近的
// 同余值（兼容回绕与窗口内乱序）。
func extendSeq(head uint64, init bool, seq uint16) uint64 {
	if !init {
		return uint64(seq)
	}
	cand := (head &^ 0xFFFF) | uint64(seq)
	best, bestDist := cand, absDiff(cand, head)
	for _, alt := range [2]uint64{cand - 1<<16, cand + 1<<16} {
		if d := absDiff(alt, head); d < bestDist {
			best, bestDist = alt, d
		}
	}
	return best
}

func absDiff(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return b - a
}
