// Package gateway 客户端事件面（#32）：seq 追踪、断线续传、invalid_session 全量补拉。
//
// 与 voice.Session 解耦（只依赖 internal/signal 的信封类型）；**禁止 import Wails**
// （#34 已整包移入 core/gateway，纯移动）。
//
// 时序（同会话重连）：client.hello → server.hello{version,session_id} 确认同会话 →
// gateway.resume{session_id,last_seq} → 环内缺失事件按 seq 升序补发 → 续流。
// 补不上 → gateway.invalid_session{reason} → 回调 OnResync 走 REST 全量补拉。
// 老服务端不发 server.hello/seq，整个包处于旁路状态，行为与 v1 完全一致。
package gateway

import (
	"sort"
	"sync"

	"github.com/OpenALC/LiteTalk/client/internal/signal"
)

// maxPending 乱序缓冲上限：正常路径 FIFO 顺序（缓冲瞬时即清），积压到上限说明
// 缺失段补不上（环滚过且 invalid_session 也丢了等异常），转全量补拉兜底。
const maxPending = 512

// Tracker 事件面 seq 追踪与续传状态。Gate 做去重/重排，OnServerHello 决定续传。
type Tracker struct {
	mu        sync.Mutex
	sessionID string                    // 当前会话（server.hello.session_id）
	lastSeq   int64                     // 已投递的最大 seq（0=尚未投递任何事件）
	pending   map[int64]signal.Envelope // 乱序缓冲：seq → 信封（同 seq 覆盖即去重）
	onResync  func(reason string)       // 续传失败/缓冲溢出 → 全量同步
}

// New 创建空追踪器：一个登录会话一个实例——网络重连复用同一实例记住 last_seq，
// 重新登录建新实例（新会话的 seq 从头计，不能拿旧基准）。
func New() *Tracker {
	return &Tracker{pending: make(map[int64]signal.Envelope)}
}

// OnResync 注册全量补拉回调（可选）。回调在**独立 goroutine** 内触发——
// 实现侧可安全做 REST I/O（WS 读循环禁止阻塞）。
func (t *Tracker) OnResync(fn func(reason string)) {
	t.mu.Lock()
	t.onResync = fn
	t.mu.Unlock()
}

// SessionID 当前会话 ID（未协商到为空）。
func (t *Tracker) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// LastSeq 已投递的最大事件序号（0=一条未投递）。
func (t *Tracker) LastSeq() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSeq
}

// OnServerHello 处理 server.hello（#29 协商结果）：记录会话并决定是否续传。
//   - 同会话且协议 >= 2 → 返回 (resume 载荷, true)，调用方发 gateway.resume；
//   - 首次协商 → 只记录会话，不续传（没有可回放的基准）；
//   - 会话被换掉（防御路径：同实例内 sid 本应恒定）→ 序号基准作废并触发全量补拉；
//   - 协议 < 2 或无会话（旧令牌）→ 不续传。
func (t *Tracker) OnServerHello(sessionID string, version int64) (signal.GatewayResumeData, bool) {
	t.mu.Lock()
	if sessionID != "" && t.sessionID != "" && sessionID != t.sessionID {
		t.pending = make(map[int64]signal.Envelope)
		t.lastSeq = 0
		t.sessionID = sessionID
		t.mu.Unlock()
		t.fireResync("session_changed")
		return signal.GatewayResumeData{}, false
	}
	if t.sessionID == "" {
		// 首次协商：记录会话即可——本连接之前的事件不可能有缺口
		//（会话自本连接起才存在），从 seq 1 起连续到达，无需续传。
		t.sessionID = sessionID
		t.mu.Unlock()
		return signal.GatewayResumeData{}, false
	}
	sid := t.sessionID
	last := t.lastSeq
	t.mu.Unlock()
	if sid == "" || version < 2 {
		return signal.GatewayResumeData{}, false
	}
	return signal.GatewayResumeData{SessionID: sid, LastSeq: last}, true
}

// Gate 作为 signal.Config.Gate（#32）：对每条下行信封返回应投递给订阅者的信封
// （按返回序投递；通常 0 或 1 条，补发窗口的乱序回填可能一次多条）。
//
//   - seq<=0 的消息（语音面/连接控制）直通；gateway.invalid_session 由本包消化：
//     重置序号基准、投递在途事件、触发全量补拉，不回投给订阅者。
//   - seq>0 的事件面消息：同 seq 覆盖即去重；从 lastSeq+1 起连续段立即投递，
//     有空洞先缓冲（补发/重连交错时乱序到达），空洞补齐后自动释放。
func (t *Tracker) Gate(env signal.Envelope) []signal.Envelope {
	if env.Type == signal.TypeGatewayInvalidSession {
		return t.handleInvalid("invalid_session")
	}
	if env.Seq <= 0 {
		return []signal.Envelope{env}
	}
	t.mu.Lock()
	if env.Seq <= t.lastSeq {
		t.mu.Unlock()
		return nil // 已投递过（补发与实时投递可能重叠）
	}
	t.pending[env.Seq] = env
	over := len(t.pending) > maxPending
	if over {
		out := t.resetLocked()
		t.mu.Unlock()
		t.fireResync("pending_overflow")
		return out
	}
	out := t.flushLocked()
	t.mu.Unlock()
	return out
}

// handleInvalid 续传失败/流失序：投递在途事件（消费者按 id 幂等），
// 重置序号基准让新流（服务端环可能已从 1 重计）能继续，并异步触发全量补拉。
// 返回值是要回投的在途事件；invalid_session 本身不回投。
func (t *Tracker) handleInvalid(reason string) []signal.Envelope {
	t.mu.Lock()
	out := t.resetLocked()
	t.mu.Unlock()
	t.fireResync(reason)
	return out
}

// resetLocked 清空缓冲与序号基准（调用方持锁）。返回清空前按序收拢的在途事件。
func (t *Tracker) resetLocked() []signal.Envelope {
	out := t.sortedPendingLocked()
	t.pending = make(map[int64]signal.Envelope)
	t.lastSeq = 0
	return out
}

// flushLocked 释放连续可投递段：lastSeq+1 起连续的缓冲项（调用方持锁）。
func (t *Tracker) flushLocked() []signal.Envelope {
	var out []signal.Envelope
	for {
		next, ok := t.pending[t.lastSeq+1]
		if !ok {
			return out
		}
		delete(t.pending, t.lastSeq+1)
		t.lastSeq++
		out = append(out, next)
	}
}

// sortedPendingLocked 按 seq 升序收拢缓冲（调用方持锁）。
func (t *Tracker) sortedPendingLocked() []signal.Envelope {
	if len(t.pending) == 0 {
		return nil
	}
	out := make([]signal.Envelope, 0, len(t.pending))
	for _, env := range t.pending {
		out = append(out, env)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// fireResync 异步触发全量补拉（不得在 WS 读循环上做 REST I/O）。
func (t *Tracker) fireResync(reason string) {
	t.mu.Lock()
	fn := t.onResync
	t.mu.Unlock()
	if fn == nil {
		return
	}
	go fn(reason)
}
