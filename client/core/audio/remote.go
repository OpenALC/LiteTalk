package audio

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/codec"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
	"github.com/pion/webrtc/v4"
)

// PeerVoiceState 对端语音状态（voice.state 广播的接收端视图，M3 UI 事件源）。
type PeerVoiceState struct {
	Speaking bool
	Mute     bool
	Deafen   bool
}

// StateTable 对端语音状态表：信令层喂入增量，M3 经 OnChange/OnRemove 接 UI。
// 全部方法并发安全；回调在调用方 goroutine 上同步触发，不得阻塞。
type StateTable struct {
	mu     sync.RWMutex
	states map[string]PeerVoiceState

	// OnChange 某对端状态变化（含首次出现）。
	OnChange func(peerID string, st PeerVoiceState)
	// OnRemove 某对端离开。
	OnRemove func(peerID string)
}

// NewStateTable 创建状态表。
func NewStateTable() *StateTable {
	return &StateTable{states: make(map[string]PeerVoiceState)}
}

// Apply 应用增量更新（nil 字段不变）。
func (t *StateTable) Apply(peerID string, speaking, mute, deafen *bool) {
	t.mu.Lock()
	st := t.states[peerID]
	if speaking != nil {
		st.Speaking = *speaking
	}
	if mute != nil {
		st.Mute = *mute
	}
	if deafen != nil {
		st.Deafen = *deafen
	}
	t.states[peerID] = st
	cb := t.OnChange
	t.mu.Unlock()
	if cb != nil {
		cb(peerID, st)
	}
}

// Set 全量写入某对端状态（channel.joined 成员列表用）。
func (t *StateTable) Set(peerID string, st PeerVoiceState) {
	t.mu.Lock()
	t.states[peerID] = st
	cb := t.OnChange
	t.mu.Unlock()
	if cb != nil {
		cb(peerID, st)
	}
}

// Remove 移除某对端（peer 离开）。
func (t *StateTable) Remove(peerID string) {
	t.mu.Lock()
	_, ok := t.states[peerID]
	delete(t.states, peerID)
	cb := t.OnRemove
	t.mu.Unlock()
	if ok && cb != nil {
		cb(peerID)
	}
}

// Clear 清空全部对端状态并逐条触发 OnRemove（退房/会话关闭用）。
// 直接换新 map 而不是逐键 delete：Go map 不会因删除而收缩桶，换表才能
// 把历史成员占用的桶真正交给 GC。回调在锁外触发，允许回调再调本表。
func (t *StateTable) Clear() {
	t.mu.Lock()
	old := t.states
	t.states = make(map[string]PeerVoiceState)
	cb := t.OnRemove
	t.mu.Unlock()
	if cb == nil {
		return
	}
	for id := range old {
		cb(id)
	}
}

// Get 查询某对端状态。
func (t *StateTable) Get(peerID string) (PeerVoiceState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	st, ok := t.states[peerID]
	return st, ok
}

// Snapshot 全量快照（M3 初始化 UI 用）。
func (t *StateTable) Snapshot() map[string]PeerVoiceState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]PeerVoiceState, len(t.states))
	for k, v := range t.states {
		out[k] = v
	}
	return out
}

// RemoteStats 单路远端链路统计（原子计数 + 抖动缓冲快照）。
type RemoteStats struct {
	RTPReceived  atomic.Uint64 // 收到的 RTP 包数
	FramesPopped atomic.Uint64 // jitter 弹出的正常帧数
	PLCFrames    atomic.Uint64 // 无 FEC 可用时的 PLC 补偿帧数
	FECFrames    atomic.Uint64 // 用后继包带内 FEC 恢复的丢帧数
	DecodeErrors atomic.Uint64
}

// remotePeer 单路远端：track 读循环 → jitter → 定时 pop → 解码（PLC）→ 混音输入。
type remotePeer struct {
	id    string
	track *webrtc.TrackRemote
	jb    *rtc.JitterBuffer
	dec   *codec.Decoder
	input *PeerInput

	done chan struct{}
	wg   sync.WaitGroup

	Stats RemoteStats

	// frameMS 当前频道 Opus 帧长（10/20ms，#46）：决定弹帧节拍与 PLC 合成长度。
	// 由 RemoteManager.SetFrameMS 统一写入（同一语音频道配置一致）。
	frameMS atomic.Int32

	// 预分配热路径缓冲（仅解码循环使用）
	payloadBuf [rtc.JitterMaxPayload]byte
	fecBuf     [rtc.JitterMaxPayload]byte // 丢帧瞬间的后继包副本（FEC 用）
	pcm        [OpusFrameSize]int16
}

// RemoteManager 远端链路装配器：OnTrack → 每路独立 jitter+解码 goroutine →
// 写入该 peer 的混音输入环；peer 离开（信令或 track  EOF）销毁。
type RemoteManager struct {
	mixer *Mixer
	log   *log.Logger

	// frameMS 当前频道帧长（10/20ms，#46）。新接入的远端轨继承此值；
	// 热切换时同步写入全部在管远端。
	frameMS atomic.Int32

	mu    sync.Mutex
	peers map[string]*remotePeer
}

// NewRemoteManager 创建装配器。
func NewRemoteManager(m *Mixer, logger *log.Logger) *RemoteManager {
	if logger == nil {
		logger = log.New(nopWriter{}, "", 0)
	}
	rm := &RemoteManager{mixer: m, log: logger, peers: make(map[string]*remotePeer)}
	rm.frameMS.Store(FrameMS20)
	return rm
}

// SetFrameMS 热切换下行帧长（10/20ms，#46 频道配置）：后续弹帧节拍与 PLC
// 帧长随之改变。切换不重置抖动缓冲——已缓冲的旧帧按包内时长解码，过渡期
// 最多一拍的节拍偏差。
func (rm *RemoteManager) SetFrameMS(ms int) error {
	if ms != FrameMS10 && ms != FrameMS20 {
		return fmt.Errorf("帧长非法: %d（只支持 10/20ms）", ms)
	}
	rm.frameMS.Store(int32(ms))
	rm.mu.Lock()
	for _, rp := range rm.peers {
		rp.frameMS.Store(int32(ms))
	}
	rm.mu.Unlock()
	return nil
}

// FrameMS 当前下行帧长（10/20ms）。
func (rm *RemoteManager) FrameMS() int { return int(rm.frameMS.Load()) }

// identifyPeer 从下行轨识别说话成员（与信令 user_id 对齐，供 StateTable/
// peer_left 对账）：服务端约定 TrackID="audio-<user_id>"、StreamID=
// "litetalk-<room>"；兼容早期语义（StreamID=user_id），最终退化为 SSRC。
func identifyPeer(track *webrtc.TrackRemote) string {
	const audioPrefix = "audio-"
	const roomPrefix = "litetalk-"
	if id := track.ID(); strings.HasPrefix(id, audioPrefix) && len(id) > len(audioPrefix) {
		return id[len(audioPrefix):]
	}
	if id := track.StreamID(); id != "" && !strings.HasPrefix(id, roomPrefix) {
		return id
	}
	if id := track.ID(); id != "" {
		return id
	}
	return fmt.Sprintf("ssrc-%d", track.SSRC())
}

// HandleTrack 接入一条远端轨（rtc.Peer 的 OnTrack 回调直挂此处）。
// peerID 见 identifyPeer。同 peerID 重复到达时拆除旧链路重建（重协商场景）。
func (rm *RemoteManager) HandleTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	peerID := identifyPeer(track)

	dec, err := codec.NewDecoder(SampleRate, 1)
	if err != nil {
		rm.log.Printf("remote: create decoder for %s: %v", peerID, err)
		return
	}

	rp := &remotePeer{
		id:    peerID,
		track: track,
		jb:    rtc.NewJitterBuffer(),
		dec:   dec,
		done:  make(chan struct{}),
	}
	rp.frameMS.Store(rm.frameMS.Load())

	rm.mu.Lock()
	if old, ok := rm.peers[peerID]; ok {
		old.stop() // 同 peer 重协商：拆旧建新（输入环复用）
	}
	rp.input = rm.mixer.AddPeer(peerID) // 与 RemovePeer 同持 rm.mu，避免误销重建的输入
	rm.peers[peerID] = rp
	rm.mu.Unlock()

	rm.log.Printf("remote: track up for %s (ssrc=%d, pt=%d)", peerID, track.SSRC(), track.PayloadType())
	rp.wg.Add(2)
	go rp.readLoop(rm)
	go rp.decodeLoop()
}

// RemovePeer 销毁某对端链路（channel.peer_left / 离房时调用），连同该 peer 的
// 混音输入一并销毁。幂等；锁序固定 rm.mu → mixer.mu。
func (rm *RemoteManager) RemovePeer(peerID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rp, ok := rm.peers[peerID]
	if !ok {
		return
	}
	delete(rm.peers, peerID)
	rp.stop()
	rm.mixer.RemovePeer(peerID)
	rm.log.Printf("remote: peer %s removed", peerID)
}

// removeIf track EOF 自愈拆除：仅当 map 中仍是本实例时拆除（不误伤重协商
// 后的新链路）。由 readLoop 异步调用（避免 stop 自我等待）。
func (rm *RemoteManager) removeIf(rp *remotePeer) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.peers[rp.id] != rp {
		return
	}
	delete(rm.peers, rp.id)
	rp.stop()
	rm.mixer.RemovePeer(rp.id)
	rm.log.Printf("remote: peer %s track ended, removed", rp.id)
}

// RemoveAll 销毁全部对端链路（离开频道/断开时调用）。
func (rm *RemoteManager) RemoveAll() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	peers := rm.peers
	rm.peers = make(map[string]*remotePeer)
	for id, rp := range peers {
		rp.stop()
		rm.mixer.RemovePeer(id)
	}
}

// PeerIDs 当前在管的远端 peer 列表。
func (rm *RemoteManager) PeerIDs() []string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	ids := make([]string, 0, len(rm.peers))
	for id := range rm.peers {
		ids = append(ids, id)
	}
	return ids
}

// JitterStats 查询某 peer 的抖动缓冲快照（统计打印用）。
func (rm *RemoteManager) JitterStats(peerID string) (rtc.JitterStats, bool) {
	rm.mu.Lock()
	rp, ok := rm.peers[peerID]
	rm.mu.Unlock()
	if !ok {
		return rtc.JitterStats{}, false
	}
	return rp.jb.Stats(), true
}

// RemoteLinkStats 单路远端链路诊断快照（#95）：抖动缓冲、FEC/PLC 计数与
// 混音输入环占用。全部为原子/快照读取，不在音频热路径上。
type RemoteLinkStats struct {
	Jitter         rtc.JitterStats
	RTPReceived    uint64
	FramesPopped   uint64
	PLCFrames      uint64
	FECFrames      uint64
	DecodeErrors   uint64
	RingAvailable  int    // 该 peer 混音输入环当前可读采样数
	UnderrunFrames uint64 // 混音 tick 读到不足的帧数
	WrittenSamples uint64
	DroppedSamples uint64
}

// LinkStats 查询某 peer 的下行链路诊断快照。
func (rm *RemoteManager) LinkStats(peerID string) (RemoteLinkStats, bool) {
	rm.mu.Lock()
	rp, ok := rm.peers[peerID]
	rm.mu.Unlock()
	if !ok {
		return RemoteLinkStats{}, false
	}
	st := RemoteLinkStats{
		Jitter:       rp.jb.Stats(),
		RTPReceived:  rp.Stats.RTPReceived.Load(),
		FramesPopped: rp.Stats.FramesPopped.Load(),
		PLCFrames:    rp.Stats.PLCFrames.Load(),
		FECFrames:    rp.Stats.FECFrames.Load(),
		DecodeErrors: rp.Stats.DecodeErrors.Load(),
	}
	if rp.input != nil {
		st.RingAvailable = rp.input.BufferedSamples()
		st.UnderrunFrames = rp.input.UnderrunFrames.Load()
		st.WrittenSamples = rp.input.WrittenSamples.Load()
		st.DroppedSamples = rp.input.DroppedSamples.Load()
	}
	return st, true
}

// stop 停止并回收单路（解码器、goroutine、混音输入）。
func (rp *remotePeer) stop() {
	close(rp.done)
	if rp.track != nil {
		_ = rp.track.SetReadDeadline(time.Now()) // 解除 readLoop 阻塞
	}
	rp.wg.Wait()
	_ = rp.dec.Close()
}

// readLoop 阻塞读 RTP → 推 jitter（每路唯一写者）。
func (rp *remotePeer) readLoop(rm *RemoteManager) {
	defer rp.wg.Done()
	for {
		pkt, _, err := rp.track.ReadRTP()
		if err != nil {
			select {
			case <-rp.done: // 本地关停解除的阻塞
			default:
				rm.log.Printf("remote: read %s: %v", rp.id, err)
				go rm.removeIf(rp) // track EOF：异步拆除本路
			}
			return
		}
		rp.Stats.RTPReceived.Add(1)
		rp.jb.Push(pkt.SequenceNumber, pkt.Payload, time.Now())
	}
}

// decodeLoop 按当前帧长驱动 decodeOnce（20ms 或 10ms，可热切换）。
func (rp *remotePeer) decodeLoop() {
	defer rp.wg.Done()
	timer := time.NewTimer(rp.popInterval())
	defer timer.Stop()
	for {
		select {
		case <-rp.done:
			return
		case <-timer.C:
		}
		rp.decodeOnce()
		timer.Reset(rp.popInterval())
	}
}

// frameMSValue 当前帧长（零值按默认 20ms，测试可直接构造 remotePeer）。
func (rp *remotePeer) frameMSValue() int {
	if int(rp.frameMS.Load()) == FrameMS10 {
		return FrameMS10
	}
	return FrameMS20
}

// popInterval 弹帧节拍 = 当前帧长。
func (rp *remotePeer) popInterval() time.Duration {
	return time.Duration(rp.frameMSValue()) * time.Millisecond
}

// frameSamples 当前帧长的 48kHz 采样数（10ms=480，20ms=960）。
func (rp *remotePeer) frameSamples() int {
	return rp.frameMSValue() * SampleRate / 1000
}

// decodeOnce 单拍：到点 pop → 解码/丢帧补偿 → 写该 peer 的混音输入环。
// 丢帧时若后继包已在 jitter 槽内，先用它的带内 FEC 恢复丢失帧（不消费后继，
// 也不做常驻 +1 包 lookahead，稳态延迟不变）；连续丢包/后继未到/后继是 DTX
// 心跳包时回落 PLC。PLC 按当前帧长合成（10ms=480、20ms=960，#46）。
// 热路径零分配（预分配 payloadBuf/fecBuf/pcm）；测试可同步调用。
func (rp *remotePeer) decodeOnce() {
	n, lost, ok := rp.jb.Pop(rp.payloadBuf[:])
	if !ok {
		return // 预填中/断流静默：本拍不出帧，混音侧自然补零
	}
	if lost {
		// 只在丢帧瞬间窥视后继槽位；DTX 包（≤2 字节）不含 FEC 数据。
		if nn, ok := rp.jb.PeekNext(rp.fecBuf[:]); ok && nn > rtc.DTXPayloadMax {
			if m, err := rp.dec.Decode(rp.fecBuf[:nn], rp.pcm[:], true); err == nil {
				rp.Stats.FECFrames.Add(1)
				rp.input.Write(rp.pcm[:m])
				return
			}
		}
		m, err := rp.dec.Decode(nil, rp.pcm[:rp.frameSamples()], false) // PLC：按当前帧长合成
		if err != nil {
			rp.Stats.DecodeErrors.Add(1)
			return
		}
		rp.Stats.PLCFrames.Add(1)
		rp.input.Write(rp.pcm[:m])
		return
	}
	m, err := rp.dec.Decode(rp.payloadBuf[:n], rp.pcm[:], false)
	if err != nil {
		rp.Stats.DecodeErrors.Add(1)
		return
	}
	rp.Stats.FramesPopped.Add(1)
	rp.input.Write(rp.pcm[:m])
}

// JitterPopInterval 默认抖动缓冲弹出节拍（20ms 帧长；10ms 频道按配置动态计算）。
const JitterPopInterval = 20 * time.Millisecond

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
