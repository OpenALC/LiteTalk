package sfu

import (
	"sync"

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
	r.dropRoutesFrom(userID)
	p.close()
	r.mu.RLock()
	n := len(r.peers)
	r.mu.RUnlock()
	r.sfu.log.Printf("sfu: 成员 %s 离开房间 %s（connID %s，剩余 %d 人）", userID, r.id, connID, n)
	return true
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
func (r *Room) forward(srcUserID string, src *webrtc.TrackRemote) {
	for {
		pkt, _, err := src.ReadRTP()
		if err != nil {
			return
		}
		r.mu.RLock()
		for uid, p := range r.peers {
			if uid == srcUserID {
				continue
			}
			p.mu.Lock()
			route := p.outbound[srcUserID]
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
