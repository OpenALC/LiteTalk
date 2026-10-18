// gateway 事件面（#32）：每会话（sid）事件环 + seq 分配 + 断线续传补发。
//
// 命名空间边界（api/signal/messages/*.schema.json 的 x-namespace）：
//   - event 事件面：chat.message / presence.update / session.kicked / server.shutdown
//     / voice.speaking——登录后常驻，按「订阅」下发给该用户全部服务器的相关事件，
//     与是否在语音房无关；进环、带 seq，断线后可 gateway.resume 补发。
//   - voice 语音面：channel.* / rtc.* / voice.state* / session.voice_taken——
//     进房才产生，瞬时语义，不进环、无 seq（replay 过期状态只会污染客户端）。
//   - session 连接控制：hello 系 / error / gateway.resume / gateway.invalid_session。
//
// 单实例假设：环在进程内存（默认 SQLite 单机部署），续传是**会话粘性**的。
// 多实例部署需粘性路由或换 Redis Stream——EventLog 接口即为那个演进点，
// 换实现不动调用方。不要假装支持多实例。
package signal

import (
	"encoding/json"
	"sync"
	"time"
)

// Event 事件环条目（#32）。Payload 在 Append 前已序列化——环内不放活对象，
// Append 是 O(1) 追加 + 淘汰，不做 I/O 不二次编码。
type Event struct {
	Seq     int64
	Type    string
	Payload json.RawMessage
	TS      int64
}

// EventLog 每会话事件环（#32）。seq 按会话（sid）从 1 起单调递增。
//
//	Bind    连接建立时登记会话归属（uid → sid 索引，广播定向用）
//	Append  分配 seq 并留存；环不存在（无会话/已淘汰）返回 0，不留存
//	Replay  返回 seq >= fromSeq 的留存事件（升序）；reason 非空即续传失败
//	        （unknown_session / seq_expired，与 gateway.invalid_session.reason 对应）
//	SidsOf  该 uid 名下全部存活会话（在线 + 断线续传窗口内）
type EventLog interface {
	Bind(sid, uid string)
	Append(sid string, ev Event) int64
	Replay(sid string, fromSeq int64) (events []Event, reason string)
	SidsOf(uid string) []string
}

const (
	// defaultEventBacklog 每会话事件环条数（LITETALK_GATEWAY_BACKLOG，§0.11）。
	// 零值/缺省一律回落本值；补发窗口 = min(环容量, 空闲 TTL)。
	defaultEventBacklog = 256
	// ringIdleTTL 会话环空闲淘汰：Bind/Append/Replay 均续期。断线超过此时长
	//（或环滚过容量）即续传失败 → 客户端走 REST 全量补拉。
	ringIdleTTL = 30 * time.Minute
	// sweepEvery 空闲淘汰的触发间隔（amortized 到写入路径，不设独立 goroutine）。
	sweepEvery = time.Minute
)

// memEventLog EventLog 的进程内环形队列实现（单实例假设）。
type memEventLog struct {
	mu        sync.Mutex
	rings     map[string]*sidRing            // sid → ring
	byUID     map[string]map[string]*sidRing // uid → sid → ring（广播定向）
	cap       int
	lastSweep time.Time
}

// sidRing 单会话的事件环。buf 定长覆写，seq 单调不受覆写影响。
type sidRing struct {
	sid     string
	uid     string
	buf     []Event
	head    int // 下一个写入位置
	count   int
	nextSeq int64 // 下一个分配的 seq（从 1 起）
	last    time.Time
}

// newMemEventLog 构造内存事件环。backlog <= 0 回落 defaultEventBacklog（§0.11）。
func newMemEventLog(backlog int) *memEventLog {
	if backlog <= 0 {
		backlog = defaultEventBacklog
	}
	return &memEventLog{
		rings:     make(map[string]*sidRing),
		byUID:     make(map[string]map[string]*sidRing),
		cap:       backlog,
		lastSweep: time.Now(),
	}
}

// Bind 登记会话归属（连接建立时）。同 sid 重复绑定只刷新归属与空闲计时。
func (l *memEventLog) Bind(sid, uid string) {
	if sid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked()
	now := time.Now()
	r := l.rings[sid]
	if r == nil {
		r = &sidRing{sid: sid, uid: uid, buf: make([]Event, l.cap), nextSeq: 1, last: now}
		l.rings[sid] = r
	} else {
		r.last = now
		if uid != "" && r.uid != uid {
			r.uid = uid
		}
	}
	if r.uid != "" {
		idx := l.byUID[r.uid]
		if idx == nil {
			idx = make(map[string]*sidRing, 1)
			l.byUID[r.uid] = idx
		}
		idx[sid] = r
	}
}

// Append 分配 seq 并留存事件，返回分配的序号（0 = 无环不留存，调用方退化为无 seq 下发）。
// O(1)：定长覆写，不淘汰扫描、不二次编码。
func (l *memEventLog) Append(sid string, ev Event) int64 {
	if sid == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rings[sid]
	if r == nil {
		return 0
	}
	ev.Seq = r.nextSeq
	r.nextSeq++
	r.buf[r.head] = ev
	r.head = (r.head + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
	r.last = time.Now()
	return ev.Seq
}

// Replay 返回 seq >= fromSeq 的留存事件（升序）。reason 非空即续传失败：
//   - unknown_session：环不存在（会话未知/已空闲淘汰/服务端重启）
//   - seq_expired：fromSeq 已滚出环，或超前于环现状（重启后新环从 1 重计）
func (l *memEventLog) Replay(sid string, fromSeq int64) ([]Event, string) {
	if fromSeq < 1 {
		fromSeq = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rings[sid]
	if r == nil {
		return nil, "unknown_session"
	}
	r.last = time.Now()
	latest := r.nextSeq - 1
	if fromSeq > latest+1 {
		return nil, "seq_expired"
	}
	if r.count > 0 {
		oldest := r.nextSeq - int64(r.count)
		if fromSeq < oldest {
			return nil, "seq_expired"
		}
	}
	out := make([]Event, 0, r.count)
	// 环内最旧的元素在 head-count（mod 容量）。
	start := (r.head - r.count + len(r.buf)) % len(r.buf)
	for i := 0; i < r.count; i++ {
		ev := r.buf[(start+i)%len(r.buf)]
		if ev.Seq >= fromSeq {
			out = append(out, ev)
		}
	}
	return out, ""
}

// SidsOf 该 uid 名下全部存活会话（在线 + 断线续传窗口内）。
func (l *memEventLog) SidsOf(uid string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := l.byUID[uid]
	if len(idx) == 0 {
		return nil
	}
	out := make([]string, 0, len(idx))
	for sid := range idx {
		out = append(out, sid)
	}
	return out
}

// sweepLocked 空闲淘汰（调用方持锁）：超时环连同 uid 索引一起摘除。
// 之后同一 sid 再 resume 会得到 unknown_session → 客户端全量补拉，语义正确。
func (l *memEventLog) sweepLocked() {
	now := time.Now()
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for sid, r := range l.rings {
		if now.Sub(r.last) < ringIdleTTL {
			continue
		}
		delete(l.rings, sid)
		if idx := l.byUID[r.uid]; idx != nil {
			delete(idx, sid)
			if len(idx) == 0 {
				delete(l.byUID, r.uid)
			}
		}
	}
}

// ---- Hub 事件面发送路径 ----

// sendEvent 事件面消息投递给单连接（#32）：写入 c.sid 的环并带 seq 下发。
// 无会话（sid 空/环已淘汰）时退化为无 seq 下发，老客户端路径完全不变。
func (h *Hub) sendEvent(c *client, msgType string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		h.log.Printf("ws: 编码 %s 失败: %v", msgType, err)
		return
	}
	ts := time.Now().UnixMilli()
	seq := h.evlog.Append(c.sid, Event{Type: msgType, Payload: payload, TS: ts})
	h.deliverEvent(c, msgType, payload, ts, seq)
}

// deliverEvent 把预序列化载荷写入下行队列（seq<=0 不写信封 seq 字段）。
// 队列满 = 对端消费停滞 → CloseNow，与 sendJSON 同一策略（§0.7）。
func (h *Hub) deliverEvent(c *client, msgType string, payload json.RawMessage, ts, seq int64) {
	raw, err := MarshalEnvelopePayload(msgType, payload, ts, seq)
	if err != nil {
		h.log.Printf("ws: 编码 %s 失败: %v", msgType, err)
		return
	}
	select {
	case c.send <- raw:
	default:
		h.log.Printf("ws: %s 下行队列满，关闭连接（丢弃 %s）", c.userID, msgType)
		c.closeNow()
	}
}

// broadcastEvent 事件面广播（#32）：uids 名下的全部会话进环，在线连接实时投递。
// 订阅模型：环存活期内（断线续传窗口）的会话都收到事件留存，reconnect+resume 补发；
// 无会话连接（dev 令牌/旧版本令牌）只收实时投递，与老客户端行为一致。
// 调用方不得持有 h.mu（内含 store 查询与锁内索引访问）。
func (h *Hub) broadcastEvent(uids map[string]bool, msgType string, data any) {
	if len(uids) == 0 {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		h.log.Printf("ws: 编码 %s 失败: %v", msgType, err)
		return
	}
	ts := time.Now().UnixMilli()
	seqs := make(map[string]int64, len(uids))
	for uid := range uids {
		for _, sid := range h.evlog.SidsOf(uid) {
			seqs[sid] = h.evlog.Append(sid, Event{Type: msgType, Payload: payload, TS: ts})
		}
	}
	h.mu.Lock()
	targets := make([]*client, 0, len(uids))
	for _, c := range h.conns {
		if uids[c.userID] {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		h.deliverEvent(c, msgType, payload, ts, seqs[c.sid])
	}
}

// broadcastVoiceSpeaking 说话翻转下发（#33）：SFU 按 RFC 6464 电平统一判定后
// 经此投递网关事件面。按连接协议版本分流（裁定 #5 兼容过渡，一个版本周期）：
//   - 协议 ≥2：voice.speaking 走事件面进环带 seq，含本人（自己的声波球同源）；
//   - 协议 <2（v1/未协商）：退化为 voice.state{speaking}——老客户端只认这条路，
//     行为与今天一致（不含本人：v1 的自我指示由本地 DSP 驱动）。
//
// 调用方（SFU 判定 ticker）不持任何 SFU 锁；本函数不得反调 SFU。
func (h *Hub) broadcastVoiceSpeaking(channelID, userID string, speaking bool) {
	h.mu.Lock()
	var legacy []*client
	v2 := make(map[string]bool)
	if room := h.rooms[channelID]; room != nil {
		for uid, m := range room.members {
			if m.protoVer.Load() >= 2 {
				v2[uid] = true
			} else {
				legacy = append(legacy, m)
			}
		}
		// 成员内存态同步：channel.joined 快照里的 speaking 据此回填。
		if m := room.members[userID]; m != nil {
			m.mu.Lock()
			m.state.Speaking = &speaking
			m.mu.Unlock()
		}
	}
	h.mu.Unlock()

	if len(v2) > 0 {
		h.broadcastEvent(v2, TypeVoiceSpeaking, VoiceSpeakingData{
			ChannelID: channelID, UserID: userID, Speaking: speaking,
		})
	}
	for _, c := range legacy {
		if c.userID == userID {
			continue
		}
		sp := speaking
		h.sendJSON(c, TypeVoiceState, VoiceStateData{UserID: userID, Speaking: &sp})
	}
}

// handleGatewayResume 处理 C→S gateway.resume{session_id, last_seq}（#32）。
//
// 补发窗口 = 环容量（LITETALK_GATEWAY_BACKLOG）∩ 空闲 TTL（30min）∩ 进程存活——
// 环滚过、会话空闲淘汰、服务端重启都会导致补不上，回 gateway.invalid_session
// 让客户端走 REST 全量补拉。
//
// 目标会话必须是当前连接自己的会话（token 里的 sid）：不允许借 resume 读别的
// 会话的事件流；旧连接的事件也不会写进别的 sid 的环（Append 按 c.sid 寻址）。
// 补发用环内留存的 payload 与原 seq 直投，不重新分配序号。
func (h *Hub) handleGatewayResume(c *client, d GatewayResumeData) {
	if d.SessionID == "" || d.LastSeq < 0 {
		h.sendError(c, ErrCodeBadMessage, "gateway.resume 需要 session_id 与 last_seq>=0")
		return
	}
	if c.sid == "" || d.SessionID != c.sid {
		h.sendJSON(c, TypeGatewayInvalidSession, GatewayInvalidSessionData{Reason: "unknown_session"})
		return
	}
	events, reason := h.evlog.Replay(d.SessionID, d.LastSeq+1)
	if reason != "" {
		h.log.Printf("ws: %s resume %s 失败（%s），last_seq=%d", c.userID, d.SessionID, reason, d.LastSeq)
		h.sendJSON(c, TypeGatewayInvalidSession, GatewayInvalidSessionData{Reason: reason})
		return
	}
	for _, ev := range events {
		h.deliverEvent(c, ev.Type, ev.Payload, ev.TS, ev.Seq)
	}
	h.log.Printf("ws: %s resume %s 补发 %d 条（last_seq=%d）", c.userID, d.SessionID, len(events), d.LastSeq)
}
