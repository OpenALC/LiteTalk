package sfu

import (
	"sync"

	"github.com/pion/webrtc/v4"
)

// Room 一个语音房间（M2 内存态；M3 接入频道权限与持久化时的挂接点）。
// 路由模型：inbound track（某成员上行）→ 房间内其他成员各自的 outbound
// TrackLocalStaticRTP（每订阅者重写 SSRC/Seq/TS，payload 原样透传）。
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
func (r *Room) addPeer(userID, connID string, sig SignalSender) (*Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.peers[userID]; ok {
		delete(r.peers, userID)
		r.sfu.log.Printf("sfu: 房间 %s 成员 %s 连接替换（connID %s → %s），关闭旧连接", r.id, userID, old.connID, connID)
		go old.close()
	}
	if len(r.peers) >= r.sfu.cfg.MaxMembers {
		return nil, ErrRoomFull
	}
	p, err := newPeer(r, userID, connID, sig)
	if err != nil {
		return nil, err
	}
	r.peers[userID] = p
	r.sfu.log.Printf("sfu: 成员 %s 加入房间 %s（connID %s，当前 %d 人）", userID, r.id, connID, len(r.peers))
	return p, nil
}

// removePeerIf 仅当 peer 归属 connID 时移除并关闭其 PC（§0.9 身份核对：
// 同 uid 的新连接已顶替旧 peer 时，旧连接的清理不得误删新连接）。
// 其余成员指向它的 outbound track 惰性保留：forward 循环随 inbound 读失败退出后，自然停止写入。
func (r *Room) removePeerIf(userID, connID string) {
	r.mu.Lock()
	p, ok := r.peers[userID]
	if !ok || p.connID != connID {
		r.mu.Unlock()
		return
	}
	delete(r.peers, userID)
	r.mu.Unlock()
	p.close()
	r.mu.RLock()
	n := len(r.peers)
	r.mu.RUnlock()
	r.sfu.log.Printf("sfu: 成员 %s 离开房间 %s（connID %s，剩余 %d 人）", userID, r.id, connID, n)
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
	target.outbound[srcUserID] = track
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
			track := p.outbound[srcUserID]
			p.mu.Unlock()
			if track == nil {
				continue
			}
			// TrackLocalStaticRTP 自动重写 SSRC/Seq/TS；写失败（如对端正在重协商）丢包即可
			_ = track.WriteRTP(pkt)
		}
		r.mu.RUnlock()
	}
}
