// gateway 事件面（#32）：每会话（sid）事件环 + seq 分配 + 断线续传补发。
//
// 命名空间边界（api/signal/messages/*.schema.json 的 x-namespace）：
//   - event 事件面：chat.message / presence.update / session.kicked / server.shutdown
//     / voice.speaking——登录后常驻，按「订阅」下发给该用户全部服务器的相关事件，
//     与是否在语音房无关；带 seq、断线后可 gateway.resume 补发。例外：voice.speaking
//     高频且瞬时，即时下行但不进补发环（seq=0），避免挤掉聊天消息的补发窗口。
//   - voice 语音面：channel.* / rtc.* / voice.state* / voice.whisper / voice.priority
//     / session.voice_taken——进房才产生，瞬时语义，不进环、无 seq（replay 过期状态
//     只会污染客户端）。
//   - session 连接控制：hello 系 / error / gateway.resume / gateway.invalid_session。
//
// 单实例假设：环在进程内存（默认 SQLite 单机部署），续传是**会话粘性**的。
// 多实例部署需粘性路由或换 Redis Stream——EventLog 接口即为那个演进点，
// 换实现不动调用方。不要假装支持多实例。
package signal

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
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
//	Bind     连接建立时登记会话归属（uid → sid 索引，广播定向用）
//	Append   分配 seq 并留存；环不存在（无会话/已淘汰）返回 0，不留存。
//	         不续期空闲计时——广播会 Append 到该 uid 名下所有存活 sid（含已断线
//	         的旧会话），续期会让 sweep 永远淘汰不掉旧环
//	Replay   返回 seq >= fromSeq 的留存事件（升序）；reason 非空即续传失败
//	          （unknown_session / seq_expired，与 gateway.invalid_session.reason 对应）
//	SidsOf   该 uid 名下全部存活会话（在线 + 断线续传窗口内）
//	LatestSeq 环当前最大 seq（无环返回 0）；invalid_session 让客户端重置基准
//	Touch    活跃连接的续期（心跳/实时下行）；只有真正在线的会话才该续期
type EventLog interface {
	Bind(sid, uid string)
	Append(sid string, ev Event) int64
	Replay(sid string, fromSeq int64) (events []Event, reason string)
	SidsOf(uid string) []string
	LatestSeq(sid string) int64
	Touch(sid string)
}

const (
	// defaultEventBacklog 每会话事件环条数（LITETALK_GATEWAY_BACKLOG，§0.11）。
	// 零值/缺省一律回落本值；补发窗口 = min(环容量, 空闲 TTL)。
	defaultEventBacklog = 256
	// ringIdleTTL 会话环空闲淘汰：Bind/Replay/Touch 续期。断线且超过此时长
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
// O(1)：定长覆写，不淘汰扫描、不二次编码。**不续期 last**：续期只由 Bind/Replay/Touch
// 负责，否则广播写旧 sid 的环会让断线会话永不淘汰（每次登录泄漏一个环）。
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

// LatestSeq 环当前最大 seq（无环返回 0）。invalid_session 携带它，
// 客户端据此把序号基准对齐到服务端现状，而不是清零后永久卡在空洞上。
func (l *memEventLog) LatestSeq(sid string) int64 {
	if sid == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rings[sid]
	if r == nil {
		return 0
	}
	return r.nextSeq - 1
}

// Touch 活跃连接续期：只有真正在线（心跳存活/收到实时下行）的会话才调。
// 已淘汰的环不重建——晚到的 Touch 没有可续的对象。
func (l *memEventLog) Touch(sid string) {
	if sid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.rings[sid]; r != nil {
		r.last = time.Now()
	}
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
// 能投递说明连接当前活跃：顺带给它的环续期（Append 不再顺带续期，见 EventLog 注释）。
func (h *Hub) deliverEvent(c *client, msgType string, payload json.RawMessage, ts, seq int64) {
	raw, err := MarshalEnvelopePayload(msgType, payload, ts, seq)
	if err != nil {
		h.log.Printf("ws: 编码 %s 失败: %v", msgType, err)
		return
	}
	h.evlog.Touch(c.sid)
	h.enqueueRaw(c, raw, msgType)
}

// enqueueRaw 把已编码信封写入连接下行队列（队列满 → CloseNow，§0.7）。
// 供一次序列化、多连接下发的路径复用（如 voice.speaking）。
func (h *Hub) enqueueRaw(c *client, raw []byte, msgType string) {
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

// speakingRecipientsLocked 悄悄话生效时说话事件的收件 uid 集合（#45）：
// 目标 + 说话者本人；目标只保留仍在房间里的成员。nil = 未启用悄悄话，保持
// 原广播范围。调用方须持 h.mu；不得访问 SFU。
func speakingRecipientsLocked(room *roomState, speakerUID string) map[string]bool {
	ws, ok := room.whisper[speakerUID]
	if !ok || !ws.active {
		return nil
	}
	// 实例核对：旧连接残留的悄悄话态不得影响新连接。
	m := room.members[speakerUID]
	if m == nil || ws.connID != strconv.FormatUint(m.connID, 10) {
		return nil
	}
	allowed := make(map[string]bool, len(ws.targets)+1)
	allowed[speakerUID] = true
	for uid := range ws.targets {
		if room.members[uid] != nil {
			allowed[uid] = true
		}
	}
	return allowed
}

// allowedEqual 两套收件 uid 集合是否等价（nil = 全房）。
func allowedEqual(a, b map[string]bool) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for uid := range a {
		if !b[uid] {
			return false
		}
	}
	return true
}

// collectSpeakingRecipientsLocked 按收件 uid 集合（nil = 全房）收集 v2 连接与
// legacy 成员。事件面订阅是 uid 级：同 uid 的其他在线 v2 设备也收说话指示。
// 调用方须持 h.mu。
func (h *Hub) collectSpeakingRecipientsLocked(room *roomState, allowed map[string]bool) (v2 []*client, legacy []*client) {
	v2uids := make(map[string]bool, len(room.members))
	for uid, m := range room.members {
		if allowed != nil && !allowed[uid] {
			continue
		}
		if m.protoVer.Load() >= 2 {
			v2uids[uid] = true
		} else {
			legacy = append(legacy, m)
		}
	}
	for _, c := range h.conns {
		if v2uids[c.userID] && c.protoVer.Load() >= 2 {
			v2 = append(v2, c)
		}
	}
	return v2, legacy
}

// sendVoiceSpeaking 向已筛好的连接下发一次说话事件（不做成员态更新）。
func (h *Hub) sendVoiceSpeaking(channelID, userID string, speaking bool, v2, legacy []*client) {
	if len(v2) > 0 {
		// 一次序列化、多连接下发（seq=0：不进环、不需去重排序）。
		payload, err := json.Marshal(VoiceSpeakingData{ChannelID: channelID, UserID: userID, Speaking: speaking})
		if err != nil {
			h.log.Printf("ws: 编码 %s 失败: %v", TypeVoiceSpeaking, err)
		} else {
			raw, err := MarshalEnvelopePayload(TypeVoiceSpeaking, payload, time.Now().UnixMilli(), 0)
			if err != nil {
				h.log.Printf("ws: 编码 %s 失败: %v", TypeVoiceSpeaking, err)
			} else {
				for _, c := range v2 {
					h.enqueueRaw(c, raw, TypeVoiceSpeaking)
				}
			}
		}
	}
	for _, c := range legacy {
		if c.userID == userID {
			continue
		}
		sp := speaking
		h.sendJSON(c, TypeVoiceState, VoiceStateData{UserID: userID, Speaking: &sp})
	}
}

// broadcastVoiceSpeaking 说话翻转下发（#33）：SFU 按 RFC 6464 电平统一判定后
// 经此投递网关事件面。按连接协议版本分流（裁定 #5 兼容过渡，一个版本周期）：
//   - 协议 ≥2：voice.speaking 即时下行、**不进补发环**（seq=0），含本人（自己的
//     声波球同源）。说话翻转是高频瞬时语义，进环会在十人房里一分钟内把聊天
//     消息挤出 256 条补发窗口；断线期间错过的说话态本就无回放价值。
//   - 协议 <2（v1/未协商）：退化为 voice.state{speaking}——老客户端只认这条路，
//     行为与今天一致（不含本人：v1 的自我指示由本地 DSP 驱动）。
//
// #45：源处于悄悄话 active 时，收件人收窄为 targets + 说话者本人（按 uid，
// v2 含其多设备），非目标既不看指示、也不会因优先发言等逻辑被误导。
// 过滤只读 hub roomState 的 whisper 快照——不得反调 SFU（死锁约定）。
//
// 调用方（SFU 判定 ticker）不持任何 SFU 锁；本函数不得反调 SFU。
func (h *Hub) broadcastVoiceSpeaking(channelID, userID string, speaking bool) {
	h.mu.Lock()
	var v2, legacy []*client
	if room := h.rooms[channelID]; room != nil {
		allowed := speakingRecipientsLocked(room, userID)
		v2, legacy = h.collectSpeakingRecipientsLocked(room, allowed)
		// 成员内存态同步：channel.joined 快照里的 speaking 据此回填。
		if m := room.members[userID]; m != nil {
			m.mu.Lock()
			m.state.Speaking = &speaking
			m.mu.Unlock()
		}
	}
	h.mu.Unlock()
	h.sendVoiceSpeaking(channelID, userID, speaking, v2, legacy)
}

// syncWhisperSpeakingScope 悄悄话收件范围变化时的说话态纠正（#45）：
// 若说话者当前正在说话，只给「范围变化」的成员补发：
//   - 新进入接收范围（取消悄悄话、或目标被加入）：补 speaking=true；
//   - 离开接收范围（启用悄悄话、或目标被移除）：补 speaking=false。
//
// 没有这一步时，说话中途开启悄悄话会让非目标一直保留旧的 speaking=true
// （优先发言者还会继续误导非目标 ducking），直到下一次说话翻转才纠正。
// old/new 为 nil 表示全房；调用方传入变化前后的集合即可。
func (h *Hub) syncWhisperSpeakingScope(channelID, userID string, oldAllowed, newAllowed map[string]bool) {
	if allowedEqual(oldAllowed, newAllowed) {
		return
	}
	h.mu.Lock()
	room := h.rooms[channelID]
	if room == nil {
		h.mu.Unlock()
		return
	}
	m := room.members[userID]
	if m == nil {
		h.mu.Unlock()
		return
	}
	m.mu.Lock()
	speaking := m.state.Speaking != nil && *m.state.Speaking
	m.mu.Unlock()
	if !speaking {
		h.mu.Unlock()
		return
	}
	added := make(map[string]bool)
	removed := make(map[string]bool)
	for uid := range room.members {
		if uid == userID {
			continue // 本人始终在范围内，不需要纠正
		}
		was := oldAllowed == nil || oldAllowed[uid]
		now := newAllowed == nil || newAllowed[uid]
		switch {
		case now && !was:
			added[uid] = true
		case was && !now:
			removed[uid] = true
		}
	}
	h.mu.Unlock()
	if len(added) > 0 {
		h.sendSpeakingToUIDSet(channelID, userID, true, added)
	}
	if len(removed) > 0 {
		h.sendSpeakingToUIDSet(channelID, userID, false, removed)
	}
}

// sendSpeakingToUIDSet 向指定 uid 集合补发说话态（#45，含其在线 v2 多设备；
// legacy 只覆盖房间内连接）。不做成员态更新。
func (h *Hub) sendSpeakingToUIDSet(channelID, userID string, speaking bool, uids map[string]bool) {
	h.mu.Lock()
	room := h.rooms[channelID]
	if room == nil {
		h.mu.Unlock()
		return
	}
	sel := make(map[string]bool, len(uids))
	for uid := range uids {
		if room.members[uid] != nil {
			sel[uid] = true
		}
	}
	v2, legacy := h.collectSpeakingRecipientsLocked(room, sel)
	h.mu.Unlock()
	h.sendVoiceSpeaking(channelID, userID, speaking, v2, legacy)
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
	// invalid_session 一律带上**当前连接** sid 的环现状：客户端据此把基准对齐到
	// 服务端最新 seq（而不是清零后等一个永远不会再出现的 seq）。取不到环就是 0。
	if c.sid == "" || d.SessionID != c.sid {
		h.sendJSON(c, TypeGatewayInvalidSession, GatewayInvalidSessionData{
			Reason: "unknown_session", LatestSeq: h.evlog.LatestSeq(c.sid),
		})
		return
	}
	events, reason := h.evlog.Replay(d.SessionID, d.LastSeq+1)
	if reason != "" {
		h.log.Printf("ws: %s resume %s 失败（%s），last_seq=%d", c.userID, d.SessionID, reason, d.LastSeq)
		h.sendJSON(c, TypeGatewayInvalidSession, GatewayInvalidSessionData{
			Reason: reason, LatestSeq: h.evlog.LatestSeq(c.sid),
		})
		return
	}
	out, withheld := h.filterReplayByPerm(c, events)
	if withheld > 0 {
		// #102：环里留着失去 ViewChannel 之后不该再补发的频道事件。扣掉它们会留下
		// seq 空洞——客户端只投递 lastSeq+1 起的连续段，空一条就把后续全压进乱序
		// 缓冲直到溢出。所以不补发半截流：直接 invalid_session，让客户端走 REST
		// 全量补拉（那条路径本身按当前权限过滤）。
		h.log.Printf("ws: %s resume %s 扣留 %d 条无权事件，转全量补拉", c.userID, d.SessionID, withheld)
		h.sendJSON(c, TypeGatewayInvalidSession, GatewayInvalidSessionData{
			Reason: "seq_expired", LatestSeq: h.evlog.LatestSeq(c.sid),
		})
		return
	}
	for _, ev := range out {
		h.deliverEvent(c, ev.Type, ev.Payload, ev.TS, ev.Seq)
	}
	h.log.Printf("ws: %s resume %s 补发 %d 条（last_seq=%d）", c.userID, d.SessionID, len(out), d.LastSeq)
}

// replayChannel 补发事件的频道可见性判定结果（#102）。
type replayChannel struct {
	channelID int64
	serverID  int64
	conv      bool // 私聊/群聊：可见性以 channel_members 为准
	visible   bool
}

// filterReplayByPerm 续传补发前按**当前**权限二次过滤（#102）。
//
// 事件环记录的是「当时可见」的广播对象，权限撤销（角色/覆盖变更、被移出服务器、
// 频道可见性变化）之后环里的旧条目仍然指向该频道。这里逐条重算：
//   - 带 channel_id 的事件（chat.message / message.edited / message.deleted /
//     channel.unread / channel.audio）按当前 ViewChannel 判定，不可见即扣留；
//   - presence.update 的频道字段是「好友所在房」，与查看者权限无关（发送时已按
//     收件人过滤），但为稳妥起见同样在丢失可见性时降级成 online-only 载荷；
//   - 其它类型（session.kicked / server.shutdown / conversation.changed）无频道
//     维度，原样放过。
//
// 返回过滤后的事件与被扣留条数。同一频道的结果在本次调用内缓存，避免环里几十条
// 同频道消息重复查权限。查询故障一律按不可见处理（fail closed）：扣留的代价是
// 客户端多做一次全量补拉，放过的代价是泄露已撤销权限的频道内容。
func (h *Hub) filterReplayByPerm(c *client, events []Event) ([]Event, int) {
	if len(events) == 0 {
		return events, 0
	}
	uid, err := strconv.ParseInt(c.userID, 10, 64)
	if err != nil {
		// dev 令牌（非数字 uid）没有真实权限体系：原样补发，与实时路径一致。
		return events, 0
	}
	cache := make(map[string]replayChannel, 4)
	out := make([]Event, 0, len(events))
	withheld := 0
	for _, ev := range events {
		idStr := replayChannelID(ev)
		if idStr == "" {
			out = append(out, ev)
			continue
		}
		state, ok := cache[idStr]
		if !ok {
			state = h.replayChannelVisible(context.Background(), uid, idStr)
			cache[idStr] = state
		}
		if state.visible {
			out = append(out, ev)
			continue
		}
		if ev.Type == TypePresenceUpdate {
			// 降级成 online-only 而不是扣留：presence 事件的频道字段是对方的隐私
			// 快照，去掉频道键仍然有意义（在线状态照旧）。
			if downgraded, ok := stripPresenceChannel(ev); ok {
				out = append(out, downgraded)
				continue
			}
		}
		withheld++
	}
	return out, withheld
}

// replayChannelID 事件载荷里的频道键（无频道维度返回空串）。
func replayChannelID(ev Event) string {
	switch ev.Type {
	case TypeChatMessage:
		var d ChatMessageData
		if json.Unmarshal(ev.Payload, &d) == nil {
			return d.ChannelID
		}
	case TypeMessageEdited:
		var d MessageEditedData
		if json.Unmarshal(ev.Payload, &d) == nil {
			return d.ChannelID
		}
	case TypeMessageDeleted:
		var d MessageDeletedData
		if json.Unmarshal(ev.Payload, &d) == nil {
			return d.ChannelID
		}
	case TypeChannelUnread:
		var d ChannelUnreadData
		if json.Unmarshal(ev.Payload, &d) == nil {
			return d.ChannelID
		}
	case TypeChannelAudio:
		var d ChannelAudioData
		if json.Unmarshal(ev.Payload, &d) == nil {
			return d.ChannelID
		}
	case TypePresenceUpdate:
		var d PresenceUpdateData
		if json.Unmarshal(ev.Payload, &d) == nil {
			return d.ChannelID
		}
	}
	return ""
}

// stripPresenceChannel presence.update 去掉频道键（降级为 online-only）。
// 返回 ok=false 表示载荷解不开或本就没有频道字段。
func stripPresenceChannel(ev Event) (Event, bool) {
	var d PresenceUpdateData
	if err := json.Unmarshal(ev.Payload, &d); err != nil || d.ChannelID == "" {
		return ev, false
	}
	d.ChannelID, d.ServerID = "", ""
	payload, err := json.Marshal(d)
	if err != nil {
		return ev, false
	}
	ev.Payload = payload
	return ev, true
}

// replayChannelVisible 查看者当前是否仍可见该频道（#102）。
// 会话频道（私聊/群聊）以成员表为准，服务器频道按 ViewChannel。
// 频道不存在（已删除）按不可见处理：环里的旧条目不再有可发送的意义。
func (h *Hub) replayChannelVisible(ctx context.Context, uid int64, idStr string) replayChannel {
	chID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return replayChannel{}
	}
	state := replayChannel{channelID: chID}
	if h.chatStore == nil {
		return state
	}
	ch, err := h.chatStore.ChannelByID(ctx, chID)
	if err != nil {
		return state
	}
	state.serverID = ch.ServerID
	if ch.Scope == store.ChannelScopeDirect || ch.Scope == store.ChannelScopeGroup {
		state.conv = true
		if _, err := h.chatStore.ChannelMember(ctx, chID, uid); err == nil {
			state.visible = true
		}
		return state
	}
	if h.authz == nil {
		return state
	}
	ok, err := h.authz.CanInServer(ctx, uid, ch.ServerID, chID, store.ViewChannel)
	if err != nil {
		h.log.Printf("ws: 续传权限复核失败（用户 %d 频道 %d）: %v", uid, chID, err)
		return state
	}
	state.visible = ok
	return state
}
