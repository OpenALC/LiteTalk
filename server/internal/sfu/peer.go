package sfu

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// outRoute 一条下行路由：outbound 轨道 + 其 RTPSender（摘除时 RemoveTrack 要用）。
// 离房/ICE 失败/连接顶替必须成对摘掉（#23）：惰性保留会让 srcUserID 重入后
// ensureRoute 见旧条目跳过，订阅端拿不到新 OnTrack。
type outRoute struct {
	track  *webrtc.TrackLocalStaticRTP
	sender *webrtc.RTPSender
}

// Peer 房间中的一个成员：每客户端恰好一条 PeerConnection
// （一条上行 track，N 条下行 track，§5.2）。
//
// 重协商与防 glare：
//   - 初始协商由客户端发 offer（rtc.offer C→S），SFU 应答；
//   - 初始 answer 之后只允许 SFU 侧发 offer（OnNegotiationNeeded → 防抖 → negotiate）；
//   - negotiate 与 answer 应用互斥：非 stable 状态时置 negotiateAgain，
//     answer 应用完毕后补做一次，避免 offer 在途时叠加 offer。
type Peer struct {
	userID string
	connID string // 归属 WS 连接标识（§0.9）：LeaveIf 只关归属连接，不误清新连接
	room   *Room
	pc     *webrtc.PeerConnection
	signal SignalSender

	// debounce 合并突发 OnNegotiationNeeded（如第三人加入时多条 AddTrack）为一次 offer
	debounce func(func())

	mu                sync.Mutex
	inbound           []*webrtc.TrackRemote     // 本成员上行轨道
	outbound          map[string]*outRoute      // 源 userID → 下行路由
	pendingCandidates []webrtc.ICECandidateInit // 初始 answer 前到达的 trickle 候选
	initialAnswered   bool
	negotiateAgain    bool
	closed            bool

	// —— RFC 6464 电平与活跃门控（#33）——
	// level 最近一包的电平（0=0dB … 127=静音；-1=尚未收到）。forward 写、判定循环读。
	level atomic.Int32
	// pktSeq 上行包计数（forward 每包 +1）：判定循环对比发现停流（muted/DTX/断流）
	// 后按静音处理，避免最后一包的电平把说话态永久卡住。
	pktSeq atomic.Uint64
	// fwdActive 活跃转发门（#33 Phase 1 包级门控）：tickActive 写、forward 读。
	// true=该源的包参与扇出 WriteRTP；false=只收不发（track 保持挂载，禁 RemoveTrack）。
	fwdActive atomic.Bool

	// —— 以下仅 speakingLoop goroutine 访问（单写者，免锁）——
	effLvl       int32     // 停流折算后的有效电平（tickSpeak 写、tickActive 读）
	spkEnterRun  int       // 连续达进入门限的 tick 数
	spkExitRun   int       // 连续低于退出门限的 tick 数
	speaking     bool      // 当前说话态（翻转时经 OnSpeakingFn 下发）
	gateExitRun  int       // 连续低于退出门限的 tick 数（活跃门 500ms 宽限）
	gateHoldFrom time.Time // 进入 fwdActive 的时刻（ActiveHoldMS 最少保持）
	seenSeq      uint64    // 上次判定时的 pktSeq（停流检测）
	staleRun     int       // 连续无新包的 tick 数
}

func newPeer(room *Room, userID, connID string, sig SignalSender) (*Peer, error) {
	pc, err := room.sfu.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("创建 PeerConnection 失败: %w", err)
	}
	p := &Peer{
		userID:   userID,
		connID:   connID,
		room:     room,
		pc:       pc,
		signal:   sig,
		outbound: make(map[string]*outRoute),
		debounce: newDebouncer(50 * time.Millisecond),
	}
	// 电平初值 -1=未收到（说话判定跳过；活跃门不拦，v1 兼容）。
	// fwdActive 初值 true：首包前不丢包，tickActive 下一拍（≤100ms）再收紧。
	p.level.Store(-1)
	p.effLvl = -1
	p.fwdActive.Store(true)

	// SFU 侧 trickle：候选随收随发
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		sig.SendCandidate(c.ToJSON())
	})

	// SFU 主导的协商：AddTrack 后触发，防抖合并为一次 offer
	pc.OnNegotiationNeeded(func() {
		p.debounce(p.negotiate)
	})

	// 上行轨道到达 → 登记 + 给房间其他成员建转发路由 + 起透传循环
	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.inbound = append(p.inbound, remote)
		p.mu.Unlock()

		// RFC 6464 电平扩展（#33）：从协商到的头扩展里取 ID；未协商（v1 客户端）
		// 取 0，forward 跳过电平写入，说话判定/活跃门控对该源自动失效。
		levelExtID := audioLevelExtID(receiver.GetParameters().HeaderExtensions)

		p.room.sfu.log.Printf("sfu: %s 上行轨道就绪 kind=%s ssrc=%d（房间 %s）",
			p.userID, remote.Kind(), remote.SSRC(), p.room.id)

		// 对端 SR 等 RTCP 仅 drain，防读缓冲淤积
		go drainRTCPReceiver(receiver)
		p.room.addRoutesFrom(p.userID, remote)
		go p.room.forward(p, remote, levelExtID)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.room.sfu.log.Printf("sfu: %s 连接状态 %s（房间 %s）", p.userID, state, p.room.id)
		// ICE 失败 / 非主动关闭（对端销毁 PC、DTLS CloseNotify）→ 清理（#19）。
		// 主动 close（离房、连接顶替）不动：closed 先于 pc.Close 置位，据此分辨。
		if state != webrtc.PeerConnectionStateFailed && state != webrtc.PeerConnectionStateClosed {
			return
		}
		p.mu.Lock()
		deliberate := p.closed
		p.mu.Unlock()
		if deliberate {
			return
		}
		p.onConnFailed()
	})

	return p, nil
}

// reasonICEFailed rtc.failed.reason 的标准取值（契约 0.2 固定 "ice_failed"）。
const reasonICEFailed = "ice_failed"

// onConnFailed ICE 失败清理（#19）：按**实例**核对摘除本 peer（含其余成员指向
// 本端的下行路由，#23），并只通知本端连接 rtc.failed。同 WS 连接重入的新 peer
// 与本实例共享 connID，只有指针核对不会误杀它。其余成员的 channel.peer_left 由
// 信令层（signal/fail.go）在收到 SendFailed 后广播。
func (p *Peer) onConnFailed() {
	if !p.room.removePeerInstance(p) {
		return
	}
	p.room.sfu.log.Printf("sfu: %s 连接失败清理完成（房间 %s）", p.userID, p.room.id)
	if p.signal != nil {
		p.signal.SendFailed(p.room.id, p.userID, reasonICEFailed)
	}
}

// handleInitialOffer 处理客户端初始 offer（每个 peer 仅一次），返回 answer SDP。
func (p *Peer) handleInitialOffer(sdp string) (string, error) {
	p.mu.Lock()
	if p.initialAnswered {
		p.mu.Unlock()
		return "", errors.New("重复的初始 rtc.offer（初始协商后只允许 SFU 发 offer）")
	}
	p.mu.Unlock()

	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return "", fmt.Errorf("设置远端 offer 失败: %w", err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("创建 answer 失败: %w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("设置本地 answer 失败: %w", err)
	}

	// 补投初始协商完成前到达的 trickle 候选
	p.mu.Lock()
	pending := p.pendingCandidates
	p.pendingCandidates = nil
	p.initialAnswered = true
	p.mu.Unlock()
	for _, c := range pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			p.room.sfu.log.Printf("sfu: %s 补投 ICE 候选失败: %v", p.userID, err)
		}
	}
	return answer.SDP, nil
}

// handleAnswer 应用客户端对 SFU 重协商 offer 的 answer。
func (p *Peer) handleAnswer(sdp string) error {
	p.mu.Lock()
	if !p.initialAnswered {
		p.mu.Unlock()
		return errors.New("初始协商尚未完成，忽略 rtc.answer")
	}
	p.mu.Unlock()

	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("应用 rtc.answer 失败: %w", err)
	}

	// offer 在途期间被跳过的协商，回到 stable 后补做一次
	p.mu.Lock()
	again := p.negotiateAgain
	p.negotiateAgain = false
	p.mu.Unlock()
	if again {
		p.debounce(p.negotiate)
	}
	return nil
}

// negotiate SFU 侧发 offer（重协商唯一入口）。
func (p *Peer) negotiate() {
	p.mu.Lock()
	if p.closed || !p.initialAnswered {
		p.mu.Unlock()
		return
	}
	if p.pc.SignalingState() != webrtc.SignalingStateStable {
		// 已有 offer 在途：记账，等 answer 回来后补做
		p.negotiateAgain = true
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		p.room.sfu.log.Printf("sfu: %s 创建重协商 offer 失败: %v", p.userID, err)
		return
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		p.room.sfu.log.Printf("sfu: %s 设置重协商 offer 失败: %v", p.userID, err)
		return
	}
	p.signal.SendOffer(offer.SDP)
}

// addICECandidate 应用客户端 trickle 候选；初始 answer 前先缓存。
func (p *Peer) addICECandidate(c webrtc.ICECandidateInit) error {
	p.mu.Lock()
	if !p.initialAnswered {
		p.pendingCandidates = append(p.pendingCandidates, c)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	if err := p.pc.AddICECandidate(c); err != nil {
		return fmt.Errorf("添加 ICE 候选失败: %w", err)
	}
	return nil
}

// close 关闭 PeerConnection。inbound 读循环随之报错退出，转发自然停止。
func (p *Peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.pc.Close()
}

// drainRTCPSender 读空某 RTPSender 的 RTCP（RR/NACK 等），防接收缓冲淤积。
func drainRTCPSender(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

// drainRTCPReceiver 读空某 RTPReceiver 的 RTCP（对端 SR 等）。
func drainRTCPReceiver(receiver *webrtc.RTPReceiver) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := receiver.Read(buf); err != nil {
			return
		}
	}
}

// newDebouncer 返回一个防抖调用器：窗口期内的多次调用合并为最后一次执行。
func newDebouncer(d time.Duration) func(func()) {
	var mu sync.Mutex
	var timer *time.Timer
	return func(f func()) {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(d, f)
	}
}
