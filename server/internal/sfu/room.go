package sfu

import (
	"sort"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Room 一个语音房间（M2 内存态；M3 接入频道权限与持久化时的挂接点）。
// 路由模型：inbound track（某成员上行）→ 房间内其他成员各自的 outbound
// TrackLocalStaticRTP（每订阅者绑定侧改写 SSRC/PT；序号/时间戳与 payload 原样透传）。
type Room struct {
	id  string
	sfu *SFU

	mu    sync.RWMutex
	peers map[string]*Peer // userID → Peer

	// tickPeers 成员快照缓冲（复用免分配；仅 speakingLoop goroutine 访问）。
	tickPeers []*Peer
}

func newRoom(s *SFU, id string) *Room {
	return &Room{
		id:    id,
		sfu:   s,
		peers: make(map[string]*Peer),
	}
}

func (r *Room) isEmpty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers) == 0
}

func (r *Room) getPeer(userID string) *Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.peers[userID]
}

// addPeer 登记成员并创建其 PeerConnection。同 room 同 uid 仅允许一个 peer（§0.9）：
// 同人重入（同 connID 重协商）或新连接顶替（异 connID）都先清掉旧 peer。
// 顶替路径还要摘掉其余成员指向该 uid 的旧下行路由（#23，与 removePeerIf 同因）。
func (r *Room) addPeer(userID, connID string, sig SignalSender) (*Peer, error) {
	r.mu.Lock()
	var replaced *Peer
	if old, ok := r.peers[userID]; ok {
		delete(r.peers, userID)
		replaced = old
		r.sfu.log.Printf("sfu: 房间 %s 成员 %s 连接替换（connID %s → %s），关闭旧连接", r.id, userID, old.connID, connID)
	}
	if len(r.peers) >= r.sfu.cfg.MaxMembers {
		r.mu.Unlock()
		if replaced != nil {
			r.dropRoutesFrom(userID)
			go replaced.close()
		}
		return nil, ErrRoomFull
	}
	p, err := newPeer(r, userID, connID, sig)
	if err != nil {
		r.mu.Unlock()
		if replaced != nil {
			r.dropRoutesFrom(userID)
			go replaced.close()
		}
		return nil, err
	}
	r.peers[userID] = p
	n := len(r.peers)
	r.mu.Unlock()
	if replaced != nil {
		// 新 peer 的上行路由要等它 OnTrack 后才建（在本函数返回之后），
		// 这里先摘旧路由是安全的，不会误删重入后的新轨道。
		r.dropRoutesFrom(userID)
		go replaced.close()
	}
	r.sfu.log.Printf("sfu: 成员 %s 加入房间 %s（connID %s，当前 %d 人）", userID, r.id, connID, n)
	return p, nil
}

// removePeerIf 仅当 peer 归属 connID 时移除并关闭其 PC（§0.9 身份核对：
// 同 uid 的新连接已顶替旧 peer 时，旧连接的清理不得误删新连接），返回是否真实摘除。
// 其余成员指向它的下行轨道一并真摘（#23）：惰性保留会让 srcUserID 重入后
// ensureRoute 见旧条目跳过，订阅端已按 peer_left 拆掉读取循环却拿不到新 OnTrack。
func (r *Room) removePeerIf(userID, connID string) bool {
	r.mu.Lock()
	p, ok := r.peers[userID]
	if !ok || p.connID != connID {
		r.mu.Unlock()
		return false
	}
	delete(r.peers, userID)
	r.mu.Unlock()
	r.teardownPeer(p)
	return true
}

// removePeerInstance 按实例摘除（#19 ICE 失败路径专用）：同 WS 连接重入房会以
// 同一 connID 建新 peer，connID 核对分不清「失败的旧实例」与「重入的新实例」，
// 只有指针核对可以。返回是否真实摘除。
func (r *Room) removePeerInstance(p *Peer) bool {
	r.mu.Lock()
	if r.peers[p.userID] != p {
		r.mu.Unlock()
		return false
	}
	delete(r.peers, p.userID)
	r.mu.Unlock()
	r.teardownPeer(p)
	return true
}

// teardownPeer 摘除收尾：先摘其余成员指向它的下行路由，再关其 PC。
func (r *Room) teardownPeer(p *Peer) {
	r.dropRoutesFrom(p.userID)
	p.close()
	r.mu.RLock()
	n := len(r.peers)
	r.mu.RUnlock()
	r.sfu.log.Printf("sfu: 成员 %s 离开房间 %s（connID %s，剩余 %d 人）", p.userID, r.id, p.connID, n)
}

// dropRoutesFrom 摘除其余成员指向 srcUserID 的下行路由（离房/ICE 失败/连接顶替）。
// 只在 room.mu 读锁内快照目标，真正摘除在锁外（RemoveTrack 可能阻塞，见 dropRoute）。
func (r *Room) dropRoutesFrom(srcUserID string) {
	r.mu.RLock()
	targets := make([]*Peer, 0, len(r.peers))
	for uid, p := range r.peers {
		if uid != srcUserID {
			targets = append(targets, p)
		}
	}
	r.mu.RUnlock()
	for _, t := range targets {
		t.dropRoute(srcUserID)
	}
}

// dropRoute 删本 peer 指向 srcUserID 的下行路由并 RemoveTrack。持 target.mu 完成
// 「删条目 + RemoveTrack」整个序列（#23 陷阱：顺序必须先删条目再 RemoveTrack，
// 且不能在 room.mu 内调 pion 的 RemoveTrack——可能阻塞，会连带卡住整个房间的 forward）。
// RemoveTrack 会触发 OnNegotiationNeeded → SFU 发重协商 offer，走既有 S→C rtc.offer
// 路径，不涉及 rtc.failed。
func (p *Peer) dropRoute(srcUserID string) {
	p.mu.Lock()
	route, ok := p.outbound[srcUserID]
	if ok {
		delete(p.outbound, srcUserID)
		_ = p.pc.RemoveTrack(route.sender)
	}
	p.mu.Unlock()
	if ok {
		p.room.sfu.log.Printf("sfu: 路由 %s → %s 摘除（房间 %s）", srcUserID, p.userID, p.room.id)
	}
}

// onPeerReady 新成员初始 answer 完成后调用：
// 把既有成员的全部 inbound track 以 outbound 形式挂到新成员 PC 上（触发重协商）。
func (r *Room) onPeerReady(p *Peer) {
	type source struct {
		userID string
		tracks []*webrtc.TrackRemote
	}
	r.mu.RLock()
	var srcs []source
	for uid, q := range r.peers {
		if uid == p.userID {
			continue
		}
		q.mu.Lock()
		tracks := append([]*webrtc.TrackRemote(nil), q.inbound...)
		q.mu.Unlock()
		if len(tracks) > 0 {
			srcs = append(srcs, source{userID: uid, tracks: tracks})
		}
	}
	r.mu.RUnlock()

	for _, src := range srcs {
		for _, t := range src.tracks {
			if err := r.ensureRoute(p, src.userID, t); err != nil {
				r.sfu.log.Printf("sfu: 为新成员 %s 补发 %s 的轨道失败: %v", p.userID, src.userID, err)
			}
		}
	}
}

// addRoutesFrom 某成员新上行 track 到达后，给房间内其他成员各挂一条 outbound。
func (r *Room) addRoutesFrom(srcUserID string, src *webrtc.TrackRemote) {
	r.mu.RLock()
	targets := make([]*Peer, 0, len(r.peers))
	for uid, p := range r.peers {
		if uid != srcUserID {
			targets = append(targets, p)
		}
	}
	r.mu.RUnlock()

	for _, t := range targets {
		if err := r.ensureRoute(t, srcUserID, src); err != nil {
			r.sfu.log.Printf("sfu: 为 %s 添加 %s 的转发轨道失败: %v", t.userID, srcUserID, err)
		}
	}
}

// ensureRoute 保证 target 有一条对应 srcUserID 的 outbound TrackLocalStaticRTP。
// 幂等：已存在则跳过（OnTrack 与 onPeerReady 两条路径可能并发触发）。
func (r *Room) ensureRoute(target *Peer, srcUserID string, src *webrtc.TrackRemote) error {
	if target.userID == srcUserID {
		return nil
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.closed {
		return nil
	}
	if _, ok := target.outbound[srcUserID]; ok {
		return nil
	}
	track, err := webrtc.NewTrackLocalStaticRTP(
		src.Codec().RTPCodecCapability,
		"audio-"+srcUserID,
		"litetalk-"+r.id,
	)
	if err != nil {
		return err
	}
	sender, err := target.pc.AddTrack(track)
	if err != nil {
		return err
	}
	target.outbound[srcUserID] = &outRoute{track: track, sender: sender}
	// RTCP 读循环防淤积（SR/RR 仅 drain + 计数；NACK/PLI 不透传，音频靠 FEC/PLC）
	go drainRTCPSender(sender)
	r.sfu.log.Printf("sfu: 路由 %s → %s 建立（房间 %s）", srcUserID, target.userID, r.id)
	return nil
}

// forward 单条 inbound track 的透传循环：逐包 ReadRTP，扇出 WriteRTP 到
// 房间内其他成员的对应 outbound track。不解包、不分析 payload。
// inbound 读失败（对端 PC 关闭/轨道结束）即退出，自然停止所有转发。
//
// #33：每包先读 RFC 6464 电平写原子（说话判定循环侧消费），再过活跃门——
// 非活跃源整包丢弃但 track 保持挂载（Phase 1 包级门控；RemoveTrack 会触发
// 重协商风暴，#23 踩过，这里禁止）。循环内不加锁不分配（pion ReadRTP 除外）。
func (r *Room) forward(src *Peer, srcTrack *webrtc.TrackRemote, levelExtID uint8) {
	for {
		pkt, _, err := srcTrack.ReadRTP()
		if err != nil {
			return
		}
		if levelExtID != 0 {
			if ext := pkt.Header.GetExtension(levelExtID); len(ext) > 0 {
				src.level.Store(int32(ext[0] & 0x7F))
			}
			src.pktSeq.Add(1) // 停流检测（muted/DTX 后无包，ticker 据此按静音处理）
		}
		if !src.fwdActive.Load() {
			continue
		}
		r.mu.RLock()
		for uid, p := range r.peers {
			if uid == src.userID {
				continue
			}
			p.mu.Lock()
			route := p.outbound[src.userID]
			p.mu.Unlock()
			if route == nil {
				continue
			}
			// pion 只改写绑定侧 SSRC/PT，Seq/TS 与 payload 原样透传；写失败（如对端
			// 正在重协商、路由已被摘除）丢包即可
			_ = route.track.WriteRTP(pkt)
		}
		r.mu.RUnlock()
	}
}

// speakFlip 一次说话翻转（tickSpeak 累积后统一触发回调）。
type speakFlip struct {
	userID   string
	speaking bool
}

// activeCand 活跃门候选（tickActive 内部用）。
type activeCand struct {
	p     *Peer
	level int32
}

// snapPeers 成员快照进 r.tickPeers（复用缓冲，仅 speakingLoop goroutine 访问）。
func (r *Room) snapPeers() []*Peer {
	r.mu.RLock()
	r.tickPeers = r.tickPeers[:0]
	for _, p := range r.peers {
		r.tickPeers = append(r.tickPeers, p)
	}
	r.mu.RUnlock()
	return r.tickPeers
}

// tickSpeak 一次说话翻转判定（#33，speakingTick 周期）。仅 speakingLoop
// goroutine 调用；对 peer 快照后在锁外处理，回调（进 WS 层）不得持任何 SFU
// 锁——hub 会持自己的锁调 SFU，交叉持锁必死锁。
//
// 顺带把停流折算后的**有效电平**写进 p.effLvl（单写者免锁），供 activeTick 的
// tickActive 取用，免得活跃门再养一套停流计数。
func (r *Room) tickSpeak(onSpeak OnSpeakingFn) {
	peers := r.snapPeers()
	if len(peers) == 0 {
		return
	}

	var flips []speakFlip
	for _, p := range peers {
		lvl := p.level.Load()
		if lvl < 0 {
			// 无电平（v1 客户端/未协商扩展）：说话判定不做（v1 走 voice.state 上报
			// 广播），活跃门不生效（恒转发，兼容老客户端）。
			p.effLvl = lvl
			continue
		}
		// 停流视同静音（muted/DTX/断流后无包，最后一包电平不能把态卡住）。
		if seq := p.pktSeq.Load(); seq == p.seenSeq {
			p.staleRun++
		} else {
			p.seenSeq = seq
			p.staleRun = 0
		}
		if p.staleRun >= levelStaleTicks {
			lvl = 127
		}
		p.effLvl = lvl

		// 说话迟滞（对齐客户端 DSP：双门限 + 连续 tick 防抖）。
		switch {
		case lvl <= speakEnterLevel:
			p.spkExitRun = 0
			if !p.speaking {
				p.spkEnterRun++
				if p.spkEnterRun >= speakEnterTicks {
					p.spkEnterRun = 0
					p.speaking = true
					flips = append(flips, speakFlip{p.userID, true})
				}
			}
		case lvl > speakExitLevel:
			p.spkEnterRun = 0
			if p.speaking {
				p.spkExitRun++
				if p.spkExitRun >= speakExitTicks {
					p.spkExitRun = 0
					p.speaking = false
					flips = append(flips, speakFlip{p.userID, false})
				}
			}
		default:
			p.spkEnterRun = 0
			p.spkExitRun = 0
		}
	}

	if onSpeak != nil {
		for _, f := range flips {
			onSpeak(r.id, f.userID, f.speaking)
		}
	}
}

// tickActive 一次活跃门 top-N（#33 Phase 1，activeTick 周期）。仅 speakingLoop
// goroutine 调用。有效电平取 tickSpeak 存下的 p.effLvl。
func (r *Room) tickActive() {
	peers := r.snapPeers()
	if len(peers) == 0 {
		return
	}
	now := time.Now()
	// 活跃候选（达门限或低电平宽限内），按电平升序取 top-N。
	var cands []activeCand
	for _, p := range peers {
		lvl := p.effLvl
		if lvl < 0 {
			p.fwdActive.Store(true) // 无电平源恒转发
			continue
		}
		// 活跃门候选：达进入门限，或 active 中且低电平宽限未满。
		if lvl > speakExitLevel {
			p.gateExitRun++
		} else {
			p.gateExitRun = 0
		}
		if lvl <= speakEnterLevel || (p.fwdActive.Load() && p.gateExitRun < gateGraceTicks) {
			cands = append(cands, activeCand{p, lvl})
		}
	}
	r.rankActive(cands, peers, now)
}

// rankActive 活跃门 top-N 选取（#33 Phase 1 包级门控）：
//   - 候选按电平升序（响者优先）取前 N 为 winner；
//   - 未 active 且 winner → 置位并记保持起点；
//   - active 且非 winner：ActiveHoldMS 内保留（防 top-N 抖动），期满清位——
//     只清 fwdActive（forward 停发包），track 全程挂载，绝不 RemoveTrack。
//
// 仅 speakingLoop goroutine 调用。ActiveN<=0（门控关闭）时不动任何状态。
func (r *Room) rankActive(cands []activeCand, peers []*Peer, now time.Time) {
	if r.sfu.cfg.ActiveN <= 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].level != cands[j].level {
			return cands[i].level < cands[j].level
		}
		return cands[i].p.userID < cands[j].p.userID
	})
	winners := make(map[*Peer]bool, r.sfu.cfg.ActiveN)
	for i := 0; i < len(cands) && i < r.sfu.cfg.ActiveN; i++ {
		winners[cands[i].p] = true
	}
	hold := time.Duration(r.sfu.cfg.ActiveHoldMS) * time.Millisecond
	for _, p := range peers {
		if p.effLvl < 0 {
			continue // 无电平源恒转发，tickActive 已置位
		}
		win := winners[p]
		active := p.fwdActive.Load()
		switch {
		case !active && win:
			p.fwdActive.Store(true)
			p.gateHoldFrom = now
		case active && !win:
			if p.gateHoldFrom.IsZero() {
				p.gateHoldFrom = now // 初始即 active 的成员，保持窗从首次评估起算
			}
			if now.Sub(p.gateHoldFrom) >= hold {
				p.fwdActive.Store(false)
			}
		}
	}
}
