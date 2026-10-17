package hotkey

import (
	"sync/atomic"
	"time"
)

// Gate 是按键说话的发送门。零值即可用（等价于 ModePTT 且未按下：全拦）。
//
// 三种模式的门控来源完全不同：
//   - ModePTT：按键 + 尾包窗（按住中，或松手后尾包窗内）；
//   - ModeVoice：DSP 的 VAD，由调用方把本帧判定结果传进来；
//   - ModeOpen：无门，恒放行。
//
// 字段全部是 atomic，Allow() 只做原子读：零分配、无锁，可在音频回调里每帧调用。
// Gate 含原子字段，禁止值拷贝，一律用 *Gate 传递。
type Gate struct {
	mode      atomic.Uint32
	pressed   atomic.Bool
	tailUntil atomic.Int64 // unix nano；0 表示不在尾包窗内
}

// SetMode 切换按键说话模式。
func (g *Gate) SetMode(m Mode) { g.mode.Store(uint32(m)) }

// Press 标记 PTT 按下（按住说话）。重复调用幂等。
func (g *Gate) Press() { g.pressed.Store(true) }

// Release 标记 PTT 松开，并开启长为 tail 的尾包窗：松手后一小段时间内继续
// 放行，避免最后一个字被硬切。tail<=0 表示不补尾音，同时清掉已开启的尾包窗。
func (g *Gate) Release(tail time.Duration) {
	g.pressed.Store(false)
	if tail <= 0 {
		g.tailUntil.Store(0)
		return
	}
	g.tailUntil.Store(time.Now().Add(tail).UnixNano())
}

// Allow 返回当前帧是否允许上行。speaking 是调用方（Pipeline）刚算出的本帧
// 语音激活判定（DSP VAD 结果）。
//
// speaking 走参数而不是 Gate 自己存一份：Pipeline 本来就要算它来驱动
// OnSpeakingChange，同一个地方顺手传进来即可，不增加跨 goroutine 状态，
// 也省掉一次 atomic 往返。
func (g *Gate) Allow(speaking bool) bool {
	switch Mode(g.mode.Load()) {
	case ModeOpen:
		return true
	case ModeVoice:
		return speaking
	default: // ModePTT 及越界值
	}
	if g.pressed.Load() {
		return true
	}
	return time.Now().UnixNano() < g.tailUntil.Load()
}
