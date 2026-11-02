package audio

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// PeerRingCapacity 每路 peer 输入环容量（采样）：4096 ≈ 85ms，
// 足以吸收解码 goroutine（960/20ms 突发写）与混音 tick（480/10ms 读）的错位。
const PeerRingCapacity = 4096

// 优先发言 ducking（#45）：远端优先发言者说话时，其他远端 peer 乘 0.35。
// duck 是独立乘数，与用户 SetGain 相乘——不得覆盖用户手动音量。
// 10ms tick 下线性过渡：attack 12 拍 ≈120ms，release 20 拍 ≈200ms。
const (
	PriorityDuckGain = 0.35
	duckAttackTicks  = 12
	duckReleaseTicks = 20
)

// PeerInput 单路对端的混音输入：解码 goroutine 写 PCM，混音 tick 读。
// 增益/静音/duck 可跨 goroutine 原子修改。
type PeerInput struct {
	ID   string
	ring *RingBuffer

	gainBits atomic.Uint64 // math.Float64bits(gain)
	muted    atomic.Bool
	// duckBits duck 目标乘数（math.Float64bits；1.0 = 不压低，#45）。与 gainBits
	// 分离：用户音量保持原值，duck 只在 Tick 内叠乘。
	duckBits atomic.Uint64
	// duckCur 平滑中的当前乘数。仅 Tick goroutine 读写（单写者，免锁）。
	duckCur float64

	// flushReq 帧长热切换（#117）请求丢弃已排队的旧帧。环是 SPSC，只能由消费者
	// （Tick）执行丢弃，写侧（解码 goroutine）只能置标志；下一拍生效，最多残留
	// 一拍（10ms）的旧帧长采样。
	flushReq atomic.Bool

	// 统计（原子计数）
	UnderrunFrames atomic.Uint64 // tick 读到不足 480 采样的帧数（对端静默/断流）
	WrittenSamples atomic.Uint64 // 解码侧写入的采样数
	DroppedSamples atomic.Uint64 // 环满丢弃的采样数
}

// Write 生产者（解码 goroutine）写入 PCM，返回实际写入采样数。
// SPSC 环：只允许一个写 goroutine（本路解码循环）。
func (p *PeerInput) Write(pcm []int16) int {
	n := p.ring.Write(pcm)
	p.WrittenSamples.Add(uint64(n))
	if d := len(pcm) - n; d > 0 {
		p.DroppedSamples.Add(uint64(d))
	}
	return n
}

// SetGain 设置线性增益（1.0 不变）。
func (p *PeerInput) SetGain(g float64) { p.gainBits.Store(math.Float64bits(g)) }

// Gain 当前线性增益。
func (p *PeerInput) Gain() float64 { return math.Float64frombits(p.gainBits.Load()) }

// SetMuted 设置 per-peer 静音（静音期间仍排空该路环，解除后不播陈旧音频）。
func (p *PeerInput) SetMuted(m bool) { p.muted.Store(m) }

// Muted 当前静音状态。
func (p *PeerInput) Muted() bool { return p.muted.Load() }

// SetDucked 优先发言 ducking 开关（#45）：true 时该路目标乘数降为
// PriorityDuckGain。只改独立乘数，不动用户 Gain。
func (p *PeerInput) SetDucked(ducked bool) {
	target := 1.0
	if ducked {
		target = PriorityDuckGain
	}
	p.duckBits.Store(math.Float64bits(target))
}

// Ducked 当前 duck 目标（平滑过程中的瞬时值不对外；UI/测试看目标态）。
func (p *PeerInput) Ducked() bool {
	return math.Float64frombits(p.duckBits.Load()) < 1.0
}

// nextDuck 推进一步 10ms duck 平滑并返回本拍乘数。仅 Tick goroutine 调用；
// 纯算术、零分配。attack 快于 release，避免伴奏压得太慢、恢复拖沓。
func (p *PeerInput) nextDuck() float64 {
	target := math.Float64frombits(p.duckBits.Load())
	cur := p.duckCur
	if cur == target {
		return cur
	}
	step := (1 - PriorityDuckGain) / duckReleaseTicks
	if target < cur {
		step = (1 - PriorityDuckGain) / duckAttackTicks
	}
	if cur < target {
		cur += step
		if cur > target {
			cur = target
		}
	} else {
		cur -= step
		if cur < target {
			cur = target
		}
	}
	p.duckCur = cur
	return cur
}

// BufferedSamples 输入环当前可读采样数（诊断用；只读原子，不干扰混音）。
func (p *PeerInput) BufferedSamples() int { return p.ring.Available() }

// RequestFlush 丢弃该路已排队的陈旧采样（帧长热切换用，#117）。任意 goroutine
// 可调；实际丢弃在下一拍 Tick 上由消费者完成（环是 SPSC，写侧不能动 tail）。
func (p *PeerInput) RequestFlush() { p.flushReq.Store(true) }

// Mixer 多路混音器：N 路 peer 输入环（480 采样/10ms）→ per-peer 增益 →
// int32 求和截断 → 单一输出（管线播放环）。
//
// Tick 热路径零分配：peer 快照经 atomic.Pointer 切换，仅在 Add/Remove 时
// 重建；累加器/帧缓冲全部预分配。
type Mixer struct {
	writeOut func([]int16) int // 输出汇（通常是包了 ProcessRender 的管线 WritePlayback）

	mu       sync.Mutex
	peers    map[string]*PeerInput
	peerList atomic.Pointer[[]*PeerInput] // 无锁遍历快照

	deafen atomic.Bool // 本地耳聋：排空各路并输出静音
	// masterGain 扬声器总音量（v0.1.2；0–1.2 映射自设置页 0–120），
	// 混音求和后、截断前施加；deafen 分支不受影响。
	masterBits atomic.Uint64 // math.Float64bits(masterGain)，初值 1.0

	// ---- Tick 私有的预分配缓冲 ----
	acc   [FrameSize]int32
	frame [FrameSize]int16
	out   [FrameSize]int16

	// 统计（原子计数）
	FramesMixed    atomic.Uint64 // 已混 10ms 帧数
	ClippedSamples atomic.Uint64 // 求和截断的采样数

	done    chan struct{}
	wg      sync.WaitGroup
	started atomic.Bool
}

// NewMixer 创建混音器。writeOut 为输出汇（每 tick 写入 480 采样），
// 生产路径传包了 ProcessRender 的 pipeline.WritePlayback（同一份输出
// 既是播放内容也是 AEC 参考）。
func NewMixer(writeOut func([]int16) int) *Mixer {
	m := &Mixer{
		writeOut: writeOut,
		peers:    make(map[string]*PeerInput),
		done:     make(chan struct{}),
	}
	m.masterBits.Store(math.Float64bits(1.0))
	empty := []*PeerInput(nil)
	m.peerList.Store(&empty)
	return m
}

// AddPeer 注册一路对端输入（幂等：已存在则返回既有实例）。
func (m *Mixer) AddPeer(id string) *PeerInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[id]; ok {
		return p
	}
	p := &PeerInput{ID: id, ring: NewRingBuffer(PeerRingCapacity)}
	p.SetGain(1.0)
	p.duckCur = 1.0
	p.duckBits.Store(math.Float64bits(1.0))
	m.peers[id] = p
	m.rebuildLocked()
	return p
}

// RemovePeer 注销一路对端输入（幂等）。
func (m *Mixer) RemovePeer(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.peers[id]; !ok {
		return
	}
	delete(m.peers, id)
	m.rebuildLocked()
}

// Peer 查询一路对端输入，不存在返回 nil。
func (m *Mixer) Peer(id string) *PeerInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peers[id]
}

// PeerIDs 当前注册的对端 ID 列表。
func (m *Mixer) PeerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.peers))
	for id := range m.peers {
		ids = append(ids, id)
	}
	return ids
}

// rebuildLocked 重建无锁遍历快照（仅 Add/Remove 时调用，允许分配）。
func (m *Mixer) rebuildLocked() {
	list := make([]*PeerInput, 0, len(m.peers))
	for _, p := range m.peers {
		list = append(list, p)
	}
	m.peerList.Store(&list)
}

// SetDeafen 设置本地耳聋（全部排空并输出静音）。
func (m *Mixer) SetDeafen(d bool) { m.deafen.Store(d) }

// Deafen 当前耳聋状态。
func (m *Mixer) Deafen() bool { return m.deafen.Load() }

// SetMasterGain 扬声器总音量增益（1.0 不变；设置页 0–120 → 0–1.2），原子热调。
func (m *Mixer) SetMasterGain(g float64) { m.masterBits.Store(math.Float64bits(g)) }

// MasterGain 当前总音量增益。
func (m *Mixer) MasterGain() float64 { return math.Float64frombits(m.masterBits.Load()) }

// Tick 混一个 10ms 帧：读各路环（不足补零计 underrun）→ 增益 → int32 求和
// 截断 → 写输出汇。零堆分配；仅允许单 goroutine 调用（Run 内部或测试）。
func (m *Mixer) Tick() {
	peers := *m.peerList.Load()

	for i := range m.acc {
		m.acc[i] = 0
	}

	if m.deafen.Load() {
		for _, p := range peers {
			p.flushReq.Store(false)
			p.ring.Read(m.frame[:]) // 排空丢弃
		}
		for i := range m.out {
			m.out[i] = 0
		}
		m.writeOut(m.out[:])
		m.FramesMixed.Add(1)
		return
	}

	for _, p := range peers {
		if p.flushReq.Swap(false) {
			p.ring.Discard() // 帧长切换：丢旧帧长的排队采样，不把它当作本拍欠载
		}
		n := p.ring.Read(m.frame[:])
		if n < FrameSize {
			for i := n; i < FrameSize; i++ {
				m.frame[i] = 0
			}
			p.UnderrunFrames.Add(1)
		}
		// duck 平滑即使在静音期间也推进：解除静音时乘数已是目标值，不会突然
		// 从旧的 1.0 跳变（用户在悄悄话中取消静音的情形）。
		duck := p.nextDuck()
		if p.muted.Load() {
			continue // 已排空，不混入
		}
		g := p.Gain() * duck
		if g == 1.0 {
			for i := range m.acc {
				m.acc[i] += int32(m.frame[i])
			}
		} else {
			for i := range m.acc {
				m.acc[i] += int32(float64(m.frame[i]) * g)
			}
		}
	}

	master := m.MasterGain()
	var clipped uint64
	for i := range m.acc {
		v := m.acc[i]
		if master != 1.0 {
			v = int32(float64(v) * master)
		}
		if v > math.MaxInt16 {
			v = math.MaxInt16
			clipped++
		} else if v < math.MinInt16 {
			v = math.MinInt16
			clipped++
		}
		m.out[i] = int16(v)
	}
	if clipped > 0 {
		m.ClippedSamples.Add(clipped)
	}
	m.writeOut(m.out[:])
	m.FramesMixed.Add(1)
}

// Start 启动 10ms 混音节拍 goroutine（重复调用报错；Stop 后可再 Start）。
func (m *Mixer) Start() error {
	if !m.started.CompareAndSwap(false, true) {
		return errMixerStarted
	}
	done := make(chan struct{})
	m.done = done
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			m.Tick()
		}
	}()
	return nil
}

// Stop 停止混音节拍（幂等）。
func (m *Mixer) Stop() {
	if m.started.CompareAndSwap(true, false) {
		close(m.done)
		m.wg.Wait()
	}
}

type mixerError string

func (e mixerError) Error() string { return string(e) }

const errMixerStarted = mixerError("mixer already started")
