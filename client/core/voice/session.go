// Package voice 是客户端语音会话编排层：把 audio / rtc / signal 管线
// 装配成面向 UI 的会话（登录 → 连信令 → 入房 → 通话），并经 Go 回调
// 推送给上层（落地方案 §4.6 事件表）。
package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/gateway"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/pion/webrtc/v4"
)

// KickReasonLoginExpired 本端专用被踢原因（不上 wire）：refresh_token 被服务端
// 明确拒绝（401/403），access 已无法续期，只能回登录页重新认证；或 ws.Run 因
// 不可恢复的鉴权失败（ErrAuthFatal，如无刷新来源）退出时兜底上报。与
// signal.KickReasonLoginElsewhere（异地登录）区分——后者是 wire 契约里的取值，
// 本常量只经 OnKicked 传给 UI 决定横幅文案。
const KickReasonLoginExpired = "login_expired"

// Events 会话 → UI 的事件回调（由 app 层接到 runtime.EventsEmit）。
type Events struct {
	// OnPeerJoin 有成员进入房间（含自己首次入房的成员全量推送前的逐个通知）。
	// priority 是优先发言标记（#45，MemberInfo.priority）。
	OnPeerJoin func(userID, nickname, tag string, speaking, mute, deafen, priority bool)
	// OnPeerLeave 有成员离开。
	OnPeerLeave func(userID string)
	// OnPeerPriority 某成员优先发言标记实时变化（voice.priority，#45）。
	// 角色变更不实时通知，重进房时随 channel.joined 刷新。
	OnPeerPriority func(userID string, priority bool)
	// OnPeerSpeaking 远端说话状态变化。
	OnPeerSpeaking func(userID string, speaking, mute, deafen bool)
	// OnLatency 回环/远端延迟周期上报（毫秒）。
	OnLatency func(ms int)
	// OnVoiceStats 周期语音统计（JSON，字段见 VoiceStats）。
	OnVoiceStats func(statsJSON string)
	// OnConnection 信令/RTC 连接状态变化。
	OnConnection func(state string)
	// OnSelf 入房后回传自己的 user_id（UI 标记「我」）。
	OnSelf func(userID string)
	// OnStateRequest 收到他人发来的闭麦/静音请求（v0.1.2；接收方自行决定执行）。
	OnStateRequest func(fromID, fromNickname string, mute, deafen *bool)
	// OnVoiceUnavailable 媒体不可用（管线初始化失败，或入房后采集/播放启动失败）：
	// 音频不可用但信令照常连接。errMsg 是面向用户的中文提示，不含内部路径。
	// 线程约定：初始化失败在 Connect 的调用方 goroutine 上报；启动失败在 ws 读
	// 循环的 channel.joined 处理器内上报——实现侧不得阻塞，也不得同步 Close
	// 会话（会与读循环互等）。
	OnVoiceUnavailable func(errMsg string)
	// OnKicked 被服务端踢下线（session.kicked 消息 / WS 4001：同类设备新登录，
	// 契约 §0.5）。reason 约定值 login_elsewhere。回调可能跑在 ws 读循环
	// goroutine（会话 wg 内）：实现侧不得同步 Close 会话，须异步执行。
	OnKicked func(reason string)
	// OnVoiceTaken 同账号另一会话抢占语音（session.voice_taken，契约 §0.10）：
	// 服务端已把本端摘出房间，本地按退房清理完毕。与 OnKicked 同样可能跑在
	// ws 读循环 goroutine（会话 wg 内），实现侧不得同步 Close 会话。
	OnVoiceTaken func()
	// OnPresenceUpdate 好友在线状态变化（presence.update，#11/#92）。
	// channelID/serverID 是该好友当前语音房（同现同缺，无查看权限或不在
	// 语音房时为空串）。跑在 ws 读循环 goroutine，实现侧只做转发。
	OnPresenceUpdate func(userID string, online bool, channelID, serverID string)
	// OnConversationChanged 私聊/群聊会话变更（conversation.changed，#38 B3）：
	// 只携带会话 ID 与 action（created/updated/member_added/member_removed/
	// deleted），实现侧按 ID 重新拉取会话。跑在 ws 读循环 goroutine，实现侧
	// 只做转发，不得阻塞、不得同步 Close 会话。
	OnConversationChanged func(conversationID, action string)
	// OnMessageEdited 消息编辑广播（message.edited，#39）：频道 ID、消息 ID、
	// 新正文、编辑时间（Unix 毫秒）与提及，实现侧按 message_id 原地替换。
	// 跑在 ws 读循环 goroutine，实现侧只做转发，不得阻塞、不得同步 Close 会话。
	OnMessageEdited func(d signal.MessageEditedData)
	// OnMessageDeleted 消息软删除广播（message.deleted，#39）：实现侧按
	// message_id 就地显示墓碑。与 OnMessageEdited 同线程约定。
	OnMessageDeleted func(d signal.MessageDeletedData)
	// OnChannelUnread 频道未读/提及计数变化（channel.unread，#40）。服务端按
	// 接收者精确计算，同一事件只投递给该用户；实现侧必须整体覆盖本地该频道的
	// 计数，不能累加——自己的消息不计未读，多设备读标记与断线续传都靠它同步。
	// 跑在 ws 读循环 goroutine，实现侧只做转发，不得阻塞、不得同步 Close 会话。
	OnChannelUnread func(d signal.ChannelUnreadData)
	// OnServerShutdown 服务端优雅关闭（server.shutdown，#21）：停自动重连并提示。
	// 与 OnKicked 同线程约定。
	OnServerShutdown func(reason string)
	// OnRTCFailed 本端 PeerConnection 因 ICE 失败被 SFU 移除（rtc.failed，#19）：
	// 会话已完成本地重建（销毁 PC、清远端链路、重发 channel.join），此处只报 UI。
	// 与 OnKicked 同线程约定（ws 读循环 goroutine，实现侧不得同步 Close 会话）。
	OnRTCFailed func(channelID string)
	// OnJoinFailed 进房被服务端拒绝（channel.join 的异步 error 帧，#91）：本地
	// 房间态已回滚（InRoom=false，重连不再重发 join），媒体链路与成员表已清。
	// channelID 是失败的那次 join；快速切房时旧 join 的失败不回滚当前房间，也
	// 不会回调。与 OnKicked 同线程约定（ws 读循环 goroutine，实现侧只做转发，
	// 不得阻塞、不得同步 Close 会话）。
	OnJoinFailed func(channelID, code, message string)
	// OnResync 事件面续传失败后的全量补拉提示（#32/#78，gateway.invalid_session）：
	// 消息与好友在线态已由会话内部补拉完毕，服务器/频道等 REST 视图可能已过期，
	// UI 层应重新拉取（绑定方法 ListServers/ListChannels/ListFriends 都在）。
	// app 层固定以 data:resync 事件转发，前端收到后经绑定自取。
	// 跑在独立 goroutine，可安全触发网络请求；nil = 不通知。
	OnResync func()
	// OnWhisperAck 悄悄话目标生效确认（voice.whisper_ack，#116 协议 v4）：
	// seq 是 voice.whisper 原样回传的请求序号，active=true 表示目标集合已应用
	// 到本连接的 SFU peer（可以开麦），false 表示取消已生效（关麦）。
	// 只有仍在房内才会回调；实现侧按自己的在途 seq 过滤过期确认。
	// 跑在 ws 读循环 goroutine，实现侧只做转发，不得阻塞、不得同步 Close 会话。
	OnWhisperAck func(seq int64, active bool)
}

// VoiceStats 推给 UI 的语音房统计快照。
type VoiceStats struct {
	LatencyMS  int      `json:"latency_ms"`
	Members    int      `json:"members"`
	PeerIDs    []string `json:"peer_ids"`
	EncPackets uint64   `json:"enc_packets"`
	RtpSent    uint64   `json:"rtp_sent"`

	// 音频链路诊断（#95）：播放缓冲与下行丢包可观测性。新增字段，旧前端
	// 忽略未知键即可；Peers 为空时不输出。
	PlaybackBufferedMS int            `json:"playback_buffered_ms"`
	PlaybackUnderruns  uint64         `json:"playback_underruns"`
	PlaybackDropped    uint64         `json:"playback_dropped"`
	Peers              []PeerLinkDiag `json:"peers,omitempty"`

	// 网络链路诊断（#49 B）：真实 RTT 已在 LatencyMS；这里补入站 RTP 丢包、
	// RFC3550 抖动与自适应抖动缓冲深度。全为 0 时省略（未入房/无下行流）。
	LossPct        float64 `json:"loss_pct,omitempty"`
	RTPJitterMS    float64 `json:"rtp_jitter_ms,omitempty"`
	JitterBufferMS int     `json:"jitter_buffer_ms,omitempty"`
	JitterTargetMS int     `json:"jitter_target_ms,omitempty"`
	RTPReceived    uint32  `json:"rtp_received,omitempty"`
	RTPLost        int32   `json:"rtp_lost,omitempty"`
}

// PeerLinkDiag 单路下行链路诊断（#95）：抖动缓冲深度、丢失/FEC/PLC 计数
// 与混音输入环占用。
type PeerLinkDiag struct {
	PeerID                string  `json:"peer_id"`
	JitterMs              float64 `json:"jitter_ms"`
	JitterTargetPackets   int     `json:"jitter_target_packets"`
	JitterBufferedPackets int     `json:"jitter_buffered_packets"`
	JitterLost            uint64  `json:"jitter_lost"`
	RingSamples           int     `json:"ring_samples"`
	PLCFrames             uint64  `json:"plc_frames"`
	FECFrames             uint64  `json:"fec_frames"`
	DREDFrames            uint64  `json:"dred_frames"`
	UnderrunFrames        uint64  `json:"underrun_frames"`
	DroppedSamples        uint64  `json:"dropped_samples"`

	// 入站 RTP 与缓冲深度（#49 B）：loss_pct / rtp_jitter_ms 来自 Pion 的
	// InboundRTPStreamStats；buffer_ms/target_ms 由抖动缓冲包数按帧长换算。
	// 与上面的 jitter_ms（缓冲自身的到达抖动估计）语义不同，故分开命名。
	LossPct     float64 `json:"loss_pct,omitempty"`
	RTPJitterMS float64 `json:"rtp_jitter_ms,omitempty"`
	BufferMS    int     `json:"buffer_ms,omitempty"`
	TargetMS    int     `json:"target_ms,omitempty"`
	RTPReceived uint32  `json:"rtp_received,omitempty"`
	RTPLost     int32   `json:"rtp_lost,omitempty"`
}

// PeerView UI 侧成员视图。
type PeerView struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Tag      string `json:"tag"` // 昵称#tag 的 tag，好友请求用
	Speaking bool   `json:"speaking"`
	Mute     bool   `json:"mute"`
	Deafen   bool   `json:"deafen"`
	Priority bool   `json:"priority"` // 优先发言（#45）：接收端 ducking 与徽标用
}

// Session 一次登录后的通话会话：信令连接 + 媒体管线。
type Session struct {
	api *api.APIClient
	log *log.Logger
	ev  Events
	// token 构造时的信令令牌，仅在 APIClient 无 access 时回落（dev- 令牌/
	// 直连测试）。正常路径每次拨号都取 APIClient 当前令牌，REST/WS 侧刷新
	// 后自动生效。atomic 保并发：TokenFunc 在 ws 重连 goroutine 读。
	token atomic.Value // string
	name  string

	// audioMu 保护 audioBackend/connectBegin：后端选择只允许在 Connect 之前，
	// 两者必须成对判定，否则并发调用会出现「选完之后又被 Connect 读走」的窗口。
	audioMu      sync.Mutex
	audioBackend audio.Backend // 零值 = BackendDevice（桌面端默认）
	connectBegin bool          // Connect 已开始装配，后端不再可改

	ws        *signal.Client
	pipe      *audio.Pipeline
	mixer     *audio.Mixer
	remoteMgr *audio.RemoteManager
	states    *audio.StateTable

	// voiceUnavailable/voiceErr：媒体管线初始化失败（如 Linux 缺 Opus）。
	// 降级而非整体失败——信令照常连接，JoinChannel 明确报错供前端 toast。
	voiceUnavailable bool
	voiceErr         error

	// voiceSend 说话状态发送队列：容量 1 只保留最新值（说话状态高频翻转，
	// 积压旧状态无意义），由专用 goroutine 消费发送（音频回调禁止直发网络）。
	voiceSend    chan signal.VoiceStateData
	mediaStarted atomic.Bool // 采集/播放是否已启动（入房才启动，按需点亮麦克风）

	// 悄悄话目标下发（#45）：Wails 绑定/热键线程只写最新目标并敲一下 wake，
	// 专用 goroutine 读取后发 voice.whisper——禁止在音频回调或 Wails 线程同步
	// ws.Send。wake 容量 1 合并突发，最新值以 whisperWant 为准。
	whisperMu       sync.Mutex
	whisperWant     []string
	whisperNeedSend bool // 有变更待下发（空 want 也可能是显式取消）
	// whisperMaybe 服务端可能仍挂着非空目标（含已发成功、在途或待发）。切房后
	// 即使 want 已清空，也需要发一条空数组兜底；从未用过悄悄话时保持 false，
	// 避免每次入房都白发一条信令。
	whisperMaybe bool
	// whisperSeq 已分配的最新请求序号（#116，协议 v4）：每次显式下发递增，
	// 随 voice.whisper.seq 带给服务端，由其原样回传 voice.whisper_ack。
	// whisperSendSeq 是当前待发版本的序号（0 = v3 兼容路径，服务端不回 ack）；
	// 两者分开是因为 wake 合并突发时只有最新版本会被真正发送。
	whisperSeq     int64
	whisperSendSeq int64
	whisperWake    chan struct{}
	// whisperFails 本次下发已失败的次数（#117）：用于指数退避与上限，成功即清零。
	whisperFails int
	// whisperRetryTimer 退避重试的定时器（#117）：到点敲一次 wake，由发送循环
	// 重读最新目标。成功/会话关闭时停掉。
	whisperRetryTimer *time.Timer

	// #46 频道音质配置：audioCfgMu 串行化「用户设置 + 当前频道上限/帧长」的
	// 读取与应用（ApplyAudio 在 Wails 绑定线程、channel.audio 在 WS 读循环）。
	// 生效码率 = min(用户设置, 频道上限>0)；帧长只由频道配置决定，默认 20ms。
	audioCfgMu      sync.Mutex
	userBitrateKbps int // 最近一次用户设置（已解析默认值，入房不受频道上限影响）
	chanBitrateKbps int // 当前频道码率上限，0 = 跟随用户设置
	chanFrameMS     int // 当前频道帧长，10/20
	// mediaFailReported 媒体启动失败是否已上报（#80）：同一故障周期只提示一次，
	// 重入房仍会真实重试但不再重复弹 toast；启动成功后复位，下次故障可再报。
	mediaFailReported atomic.Bool
	peerRef           atomic.Pointer[rtc.Peer]
	roomID            atomic.Value // string
	muted             atomic.Bool
	deafened          atomic.Bool
	// kicked 被踢事件一次性去重（#98）：同一条不可恢复的鉴权失败可能先由
	// OnAuthFailed 回调 handleKicked（401 被拒/会话顶替），ws.Run 返回
	// ErrAuthFatal 时又被 Run goroutine 回调一次；重复事件会让 UI 重复弹
	// 横幅/重复清理。首次生效、后续直接返回。
	kicked atomic.Bool

	// joinMu 串行化进房意图与 roomID 的复合变更（#91）。JoinChannel 乐观写
	// roomID 供断线重连恢复，服务端拒收是异步 error 帧——没有关联 id，
	// 只能把错误按服务端在同一连接上的处理顺序归属给最早未结算的 join。
	// pendingJoins 是尚未收到 channel.joined / error 的 join 队列（FIFO）；
	// seq 仅用于发送失败时精确摘除自己那条，不参与错误归属。
	// 约束：error 帧无 request id，通用码（forbidden/bad_message/rate_limited/
	// quota_exceeded）在队列非空时会被当作进房失败消费，存在把同码其他错误
	//（如 rtc.offer 配额）误判的理论窗口；队列为空时一律按聊天错误转发。
	joinMu       sync.Mutex
	pendingJoins []pendingJoin
	joinSeq      uint64
	// roomPassword 当前房的入房密码（#37，joinMu 保护）：用户发起 join 时覆盖
	//（含空串）；重连/ICE 重建的 channel.join 从这里取。Leave/voice_taken/
	// 进房失败回滚即清空。不落盘、不日志。
	roomPassword string

	// protoVer 服务端协商到的协议版本（#29）：0=尚未收到 server.hello（按 v1 行为）。
	// v2 客户端据此停发 voice.state{speaking} 等版本相关行为（#33 接线）。
	protoVer atomic.Int64

	// gw 事件面追踪（#32）：seq 去重/重排、gateway.resume 决策、invalid_session 补拉。
	gw *gateway.Tracker

	// sendGate 按键说话发送门（PTT/语音激活/常开）。App 层长期持有的值对象，
	// 会话重建不换门——用户按住的 PTT 状态不能因为重连丢掉。建管线时转交
	// Pipeline，之后由 Pipeline 每帧自行 Load。
	sendGate atomic.Pointer[hotkey.Gate]

	// iceMu/iceServers 预取的 STUN/TURN 凭证缓存（#13）：setupPeer 跑在 WS 读
	// 循环的 channel.joined 处理器里，禁止网络 I/O——凭证由 JoinChannel 在可
	// 阻塞的 Wails 绑定线程预取，读循环只读缓存；缓存空则降级空 ICE 列表。
	iceMu      sync.RWMutex
	iceServers []webrtc.ICEServer

	// roomRebuildMu 串行化「拆 PC + 重发 channel.join」的重建路径（#19/#49）：
	// rtc.failed 在 WS 读循环、网络变化在网络轮询 goroutine，两者不得并发
	// 拆链路（会留下两套 join 回执互相顶替）。两处都用 TryLock——拿不到锁
	// 说明已有重建在途，读循环绝不能等待。
	roomRebuildMu sync.Mutex
	// lastNetRecover 上次网络变化重建的 UnixNano（#49 A）：接口切换过程中
	// 签名可能连续变化，冷却窗内不重复拆链路。
	lastNetRecover atomic.Int64

	mu        sync.Mutex
	members   map[string]PeerView
	selfID    string
	chatFn    func(channelID, msgID, userID, nickname, content, ts, nonce string) // ChatHandler 注入（旧签名）
	chatMsgFn func(d signal.ChatMessageData)                                      // ChatMessageHandler 注入（#39 富字段）
	seenChans map[string]bool                                                     // 本会话收过 chat.message 的频道（补拉范围）
	errFns    []func(code, message string, joinHandled bool)                      // ErrorHandlerEx 注入（mu 保护）

	// chatMu/pendingChats 聊天发送 outbox（#31）：断线/写失败时保留 nonce 与内容，
	// 重连 OnConnect 用同一个 nonce 重发；服务端按 (channel_id, user_id, nonce) 去重。
	chatMu       sync.Mutex
	pendingChats []pendingChat

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// pendingJoin 一次已发出、尚未被 channel.joined / error 结算的进房请求。
type pendingJoin struct {
	channelID string
	seq       uint64
}

// maxPendingJoins 未结算 join 队列上限。正常一次只留一条；连点/快速切房
// 会给服务端留下多条，服务端按序逐条回执或限流，队列随之清空。异常调用方
// 无限进房时只保留最近 N 次意图，避免队列无界增长。
const maxPendingJoins = 16

// NewSession 创建会话。token 为信令鉴权令牌；nickname 仅 dev 模式用作 user_id。
func NewSession(api *api.APIClient, token, nickname string, ev Events, logger *log.Logger) *Session {
	if logger == nil {
		logger = log.Default()
	}
	s := &Session{
		api:         api,
		log:         logger,
		ev:          ev,
		name:        nickname,
		members:     make(map[string]PeerView),
		seenChans:   make(map[string]bool),
		voiceSend:   make(chan signal.VoiceStateData, 1),
		whisperWake: make(chan struct{}, 1),
		gw:          gateway.New(),
		// 入房前的默认音质：与 audio.DefaultOpusConfig 的 32kbps、20ms 一致。
		userBitrateKbps: 32,
		chanFrameMS:     20,
	}
	s.token.Store(token)
	s.roomID.Store("")
	return s
}

// SetAudioBackend 选择音频后端（audio.BackendDevice / audio.BackendNull）。
// 必须在 Connect 之前调用：管线在 Connect 中按当时的选择装配，运行期换后端
// 需要重建整条媒体链路，当前不支持——Connect 之后调用返回错误。并发调用
// 与 Connect 之间由 audioMu 串行化，要么生效、要么明确报错。
func (s *Session) SetAudioBackend(b audio.Backend) error {
	if b != audio.BackendDevice && b != audio.BackendNull {
		return fmt.Errorf("unknown audio backend: %d", int(b))
	}
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	if s.connectBegin {
		return errors.New("audio backend must be set before Connect")
	}
	s.audioBackend = b
	return nil
}

// AudioBackend 返回当前选择的音频后端；Connect 之后即为管线实际生效值。
func (s *Session) AudioBackend() audio.Backend {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	return s.audioBackend
}

// Connect 建立信令连接并装配媒体管线（采集/播放入房才启动，见 startMedia）。
// 媒体管线初始化失败不再让登录整体失败：置 voiceUnavailable、发
// OnVoiceUnavailable 事件，信令照常连接（纯文字/好友功能仍可用）。
func (s *Session) Connect(serverURL string) {
	// 后端选择在装配开始时定格：之后 SetAudioBackend 一律报错（见其注释）。
	s.audioMu.Lock()
	s.connectBegin = true
	backend := s.audioBackend
	s.audioMu.Unlock()

	wsCfg := signal.Config{
		URL:    serverURL,
		Logger: s.log,
		// 每次拨号取 APIClient 当前令牌：REST 刷新后重连自动携带新值。
		// 已知过期时先单飞续期；失败不阻断本次拨号——旧令牌交给下面的握手
		// 401 路径强制刷新一次，仍然失败才停（不会无限重连）。
		TokenFunc: func(ctx context.Context) (string, error) {
			if tok := s.api.Token(); tok != "" {
				if err := s.api.EnsureAccess(ctx); err != nil {
					s.log.Printf("signal: access 预续期失败: %v", err)
				}
				if tok = s.api.Token(); tok != "" {
					return tok, nil
				}
			}
			// APIClient 无令牌（dev 令牌/直连测试）：回落构造时的令牌。
			return s.token.Load().(string), nil
		},
		// 握手 401（被顶替类已由 OnKicked 收口）：刷新一次 access 再重拨。
		// 失败按原因分三类（#84 复审 4.1、#98）：
		//   - 传输层错误与服务端临时故障（408/429/5xx）：返回 ErrAuthTransient，
		//     signal 层退避重试——网络抖动或服务端过载不代表 refresh_token 失效，
		//     不能就此永久停连；
		//   - 服务端明确 401/403：refresh_token 已失效/被吊销，走被踢链路回
		//     登录页，前端按 login_expired 提示「登录已过期」；
		//   - ErrSessionReplaced：刷新窗口里发现会话被顶替，按异地登录处理。
		// errBaseChanged / ErrNoRefreshSource 返回原错误 → 致命（换服流程已接管 /
		// 根本没有刷新来源）；Run 以 ErrAuthFatal 退出后由 Run goroutine 兜底
		// 上报 login_expired，避免登录页一直卡在「信令连接中」。
		OnAuthFailed: func(ctx context.Context) error {
			err := s.api.RefreshAccess(ctx)
			switch {
			case err == nil:
				return nil
			case errors.Is(err, api.ErrSessionReplaced):
				s.handleKicked(signal.KickReasonLoginElsewhere)
				return err
			case api.IsAuthRejected(err):
				s.log.Printf("signal: refresh_token 已被服务端拒绝，回登录页: %v", err)
				s.handleKicked(KickReasonLoginExpired)
				return err
			case api.IsTransientRefreshError(err):
				s.log.Printf("signal: 刷新 access 瞬时失败，退避重试: %v", err)
				return signal.ErrAuthTransient
			default:
				return err
			}
		},
	}
	wsCfg.OnConnect = s.handleSignalConnect
	wsCfg.OnDisconnect = func(err error) {
		if s.ev.OnConnection != nil {
			s.ev.OnConnection("disconnected")
		}
		if p := s.peerRef.Swap(nil); p != nil {
			_ = p.Close()
		}
		if s.remoteMgr != nil {
			s.remoteMgr.RemoveAll()
		}
		s.stopMedia()
	}
	// WS close 4001 / 握手 401 带 session_replaced 头：session.kicked 消息在
	// 服务端发送窗口内可能丢帧，close code 是可靠的那一半。回调跑在 ws Run
	// goroutine（会话 wg 内），app 侧 OnKicked 已异步化，此处直接调出口即可。
	wsCfg.OnKicked = func(reason string) { s.handleKicked(reason) }
	// 事件面门（#32）：seq 去重/重排由 gateway.Tracker 承担，invalid_session
	// 在门内消化并异步触发 fullResync（独立 goroutine，REST 不进读循环）。
	wsCfg.Gate = s.gw.Gate
	s.gw.OnResync(func(reason string) {
		s.log.Printf("gateway: 续传失败（%s），全量补拉", reason)
		s.fullResync()
	})
	s.ws = signal.NewClient(wsCfg)
	s.registerHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// 媒体管线：采集→降噪→Opus→(OnPacket→RTP)；下行混音→播放环。
	// 构造失败（如无 Opus 的平台）降级：跳过音频初始化，信令不受影响。
	// 后端默认 BackendDevice；无界面/容器/CI 消费方可在 Connect 前经
	// SetAudioBackend(audio.BackendNull) 改用无声卡自检后端。
	pcfg := audio.DefaultPipelineConfig()
	pcfg.Backend = backend
	pcfg.Loopback = false
	pcfg.DSP.OnSpeakingChange = func(speaking bool) {
		// DSP 回调跑在音频 goroutine 上，必须非阻塞：投递到容量 1 的
		// voiceSend，由下方专用 goroutine 发送；积压时丢弃旧值只留最新。
		// v2 停发（#33 裁定 #5）：说话态改由服务端按 RFC 6464 电平统一判定
		//（voice.speaking 回来驱动 UI）；这条上报只留给 v1 兼容路径。
		if s.protoVer.Load() >= 2 {
			return
		}
		if s.muted.Load() {
			speaking = false
		}
		select {
		case s.voiceSend <- signal.VoiceStateData{Speaking: signal.Bool(speaking)}:
		default:
		}
	}
	pcfg.OnPacket = func(pkt []byte) {
		if p := s.peerRef.Load(); p != nil {
			_ = p.SendOpus(pkt)
		}
	}
	pipe, err := audio.NewPipeline(pcfg)
	if err != nil {
		s.voiceUnavailable = true
		s.voiceErr = err
		s.log.Printf("pipeline unavailable: %v (signaling continues)", err)
		if s.ev.OnVoiceUnavailable != nil {
			s.ev.OnVoiceUnavailable(err.Error())
		}
	} else {
		s.pipe = pipe
		// 发包门：App 层在 NewSession 之后、Connect 之前注入；此处补到管线上
		// （SetSendGate 在管线已存在时也会即时生效，两条路径等价）。
		if g := s.sendGate.Load(); g != nil {
			pipe.SetSendGate(g)
		}
		// AEC 参考 = 混音器实际输出（含耳聋/欠载的零帧）：先喂 APM render，
		// 再写播放环。混音 tick 是单 goroutine，且写环非阻塞，不会拖慢节拍。
		// 播放环/声卡引入的附加延迟由 AEC3 的延迟估计吸收。
		s.mixer = audio.NewMixer(func(pcm []int16) int {
			pipe.ProcessRender(pcm)
			return pipe.WritePlayback(pcm)
		})
		s.remoteMgr = audio.NewRemoteManager(s.mixer, s.log)
		// 新接入的远端轨才在混音器里出现，入房快照里的「优先发言者正在说话」
		// 需要在这时补一次 ducking 重算，否则要等下一次说话翻转才生效（#45）。
		s.remoteMgr.OnPeerChange = s.updateDucking
		s.states = audio.NewStateTable()
		s.states.OnChange = func(id string, st audio.PeerVoiceState) {
			if s.ev.OnPeerSpeaking != nil {
				s.ev.OnPeerSpeaking(id, st.Speaking, st.Mute, st.Deafen)
			}
		}
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case st := <-s.voiceSend:
				if s.ws != nil {
					_ = s.ws.Send(signal.TypeVoiceState, st)
				}
			}
		}
	}()

	// 悄悄话目标专用发送 goroutine（#45）：只在协议 ≥3 时下发；v2 服务端
	// 不认该消息，客户端按住悄悄话键时也不应依赖它生效。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.whisperLoop(ctx)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		if err := s.ws.Run(ctx); err != nil && err != context.Canceled {
			// ErrAuthFatal：刷新被明确拒绝 / 无刷新来源 / 换服，Run 已停止重连。
			// 必须上报被踢回登录页——否则前端停在「信令连接中」永远等不到结果。
			// 已在 OnAuthFailed 里回调过的路径由 handleKicked 一次性去重（#98）。
			if errors.Is(err, signal.ErrAuthFatal) {
				s.handleKicked(KickReasonLoginExpired)
			}
			s.log.Printf("signal run: %v", err)
		}
	}()

	// #49 A：网络接口变化监听（Wi-Fi/有线/代理切换后旧 ICE 候选失效）。
	// 仅在房内触发一次轻量重建，随会话 ctx 退出。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watchNetwork(ctx)
	}()
}

// handleSignalConnect 每条（重）连成功后的订阅状态恢复：广播连接状态、重发
// channel.join、重发断线期间积压的聊天消息（同一 nonce，服务端幂等）。
// 由 signal.Client 在首次收到下行消息后调用（server.hello 已处理完，protoVer 就绪）。
func (s *Session) handleSignalConnect(c *signal.Client) {
	if s.ev.OnConnection != nil {
		s.ev.OnConnection("connected")
	}
	// 重连后若仍在房间，重发 join（channel.joined 会重建媒体链路）。
	// 同样登记 pending：重连重入也可能被拒（频道删除/权限收回/满员），
	// 不登记的话异步 error 只会弹 toast，本地仍留在「幽灵房间」里（#91）。
	if room, _ := s.roomID.Load().(string); room != "" {
		seq := s.beginJoin(room)
		if err := c.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: room, Password: s.roomPasswordCached()}); err != nil {
			// 保留 roomID 等下次重连再试（原有语义），只摘掉本次 pending，
			// 避免后续无关 error 被误归属到这次失败的发送。
			s.abortJoinSend(seq, false)
			s.log.Printf("signal: 重连重发 channel.join 失败: %v", err)
		}
	}
	s.flushPendingChats()
}

// startMedia 启动采集/播放（幂等）。未入房不启动——空闲时也跑
// 采集+RNNoise+Opus 是白耗 CPU 且 Windows 麦克风指示灯常亮。
// 启动失败不置 voiceUnavailable（重入房可重试），但必须经 OnVoiceUnavailable
// 上报：否则 UI 显示在房内、麦克风实际没起来，用户无从得知（#80）。
// 本函数跑在 ws 读循环的 channel.joined 处理器里，上报只做转发不得阻塞。
func (s *Session) startMedia() {
	if s.voiceUnavailable || s.pipe == nil {
		return
	}
	if !s.mediaStarted.CompareAndSwap(false, true) {
		return
	}
	if err := s.mixer.Start(); err != nil {
		s.mediaStarted.Store(false)
		s.reportMediaFailure("mixer", err)
		return
	}
	if err := s.pipe.Start(); err != nil {
		s.mixer.Stop()
		s.mediaStarted.Store(false)
		s.reportMediaFailure("pipeline", err)
		return
	}
	// 启动成功：复位故障周期，下次失败重新提示。
	s.mediaFailReported.Store(false)
}

// reportMediaFailure 上报媒体启动失败（#80）。日志保留底层错误（含设备名等
// 诊断信息），事件只发面向用户的中文提示，不暴露内部路径/设备枚举细节。
// 同一故障周期只上报一次：重入房重试仍会真实重试，但不会把提示刷成风暴。
func (s *Session) reportMediaFailure(stage string, err error) {
	s.log.Printf("%s start: %v", stage, err)
	if !s.mediaFailReported.CompareAndSwap(false, true) {
		return
	}
	if s.ev.OnVoiceUnavailable == nil {
		return
	}
	if stage == "mixer" {
		s.ev.OnVoiceUnavailable("音频播放启动失败，请检查扬声器")
		return
	}
	s.ev.OnVoiceUnavailable("音频设备启动失败，请检查麦克风和扬声器")
}

// stopMedia 停止采集/播放（幂等）。与 LeaveChannel/断线/Close 的清理路径
// 合并调用；Start/Stop 成对后可再 Start（重入房）。
func (s *Session) stopMedia() {
	if !s.mediaStarted.CompareAndSwap(true, false) {
		return
	}
	if s.mixer != nil {
		s.mixer.Stop()
	}
	if s.pipe != nil {
		s.pipe.Stop()
	}
}

// registerHandlers 挂信令消息处理（对照 calltest 的接线范式）。
func (s *Session) registerHandlers() {
	s.ws.On(signal.TypeHello, func(env signal.Envelope) {
		var d signal.HelloData
		if env.Decode(&d) == nil && d.User.ID != "" {
			s.selfID = d.User.ID
		}
	})
	// server.hello：协议版本协商结果（#29）。hello（初始数据）可能先到也可能后到，
	// 与本消息的相对顺序不作假设（老服务端根本不发 server.hello，按 v1 继续）。
	s.ws.On(signal.TypeServerHello, func(env signal.Envelope) {
		var d signal.ServerHelloData
		if env.Decode(&d) != nil || d.Version < 1 {
			return
		}
		s.protoVer.Store(d.Version)
		// server.hello 确认会话后发 gateway.resume（#32）：首次协商也发 last_seq=0
		//（服务端环可能已有事件，见 #81），同会话重连带已投递的 last_seq。
		// 老服务端（无 server.hello）没有续传概念，不发；发送失败按普通断线重连处理。
		if resume, ok := s.gw.OnServerHello(d.SessionID, d.Version); ok {
			if err := s.ws.Send(signal.TypeGatewayResume, resume); err != nil {
				s.log.Printf("gateway: resume 发送失败: %v", err)
			}
		}
		s.log.Printf("signal: 协商协议版本 v%d", d.Version)
	})
	s.ws.On(signal.TypeChannelJoined, func(env signal.Envelope) {
		var d signal.ChannelJoinedData
		if err := env.Decode(&d); err != nil {
			return
		}
		// 服务端回执：结算对应的 pending join（重复/广播帧不匹配则忽略）。
		matched := s.confirmJoin(d.ChannelID)
		// 迟到回执（#99）：退房后或快速切房时，旧房的 channel.joined 可能晚于
		// 新的房间意图到达。roomID 已不指向该频道就整帧丢弃——不启动媒体、
		// 不重建成员表、不发 OnSelf/OnPeerJoin、不触发 peer 重协商。startMedia
		// 幂等只能防重复启动，防不住成员表被旧房数据覆盖。
		if room, _ := s.roomID.Load().(string); room != d.ChannelID {
			if matched {
				s.log.Printf("session: 忽略迟到 channel.joined %s（当前房 %q，#99）", d.ChannelID, room)
			}
			return
		}
		// #46 频道音质：joined 是入房的权威配置（0 = 跟随设置/默认 20ms）。
		// 旧服务端不带字段时按 0/0 处理，行为与升级前一致。必须在 startMedia
		// 与 setupPeer 之前应用，首个上行包就使用正确帧长。
		bitrate, frameMS := 0, 0
		if d.AudioBitrateKbps != nil {
			bitrate = int(*d.AudioBitrateKbps)
		}
		if d.AudioFrameMs != nil {
			frameMS = int(*d.AudioFrameMs)
		}
		s.setChannelAudio(bitrate, frameMS)
		s.startMedia() // 入房才启动采集/播放
		s.mu.Lock()
		s.members = make(map[string]PeerView, len(d.Members))
		for _, m := range d.Members {
			s.members[m.UserID] = PeerView{
				UserID: m.UserID, Nickname: m.Nickname, Tag: m.Tag,
				Speaking: m.Speaking, Mute: m.Mute, Deafen: m.Deafen,
				Priority: m.Priority,
			}
			if m.UserID != d.Self.UserID && s.states != nil {
				s.states.Set(m.UserID, audio.PeerVoiceState{Speaking: m.Speaking, Mute: m.Mute, Deafen: m.Deafen})
			}
		}
		s.selfID = d.Self.UserID
		s.mu.Unlock()
		// 入房快照可能改变优先发言状态（含自己的 admin 授予）：重算 ducking。
		s.updateDucking()
		if s.ev.OnSelf != nil {
			s.ev.OnSelf(d.Self.UserID)
		}
		if s.ev.OnPeerJoin != nil {
			for _, m := range d.Members {
				s.ev.OnPeerJoin(m.UserID, m.Nickname, m.Tag, m.Speaking, m.Mute, m.Deafen, m.Priority)
			}
		}
		// 仅在无存活 peer 时建连（他人入房广播不动媒体链路）。与网络恢复/
		// ICE 重建互斥：已有重建在途时由那条路径负责建新 PC，读循环不并发
		// 再建一条（否则两次 rtc.offer 互相顶替）。
		alive := false
		if p := s.peerRef.Load(); p != nil &&
			p.ConnectionState() != webrtc.PeerConnectionStateClosed &&
			p.ConnectionState() != webrtc.PeerConnectionStateFailed {
			alive = true
		}
		if !alive {
			// 建 peer 与下面的状态补发解耦（#117）：拿不到锁说明已有重建在途，
			// 那条路径会用新的 channel.join 再触发一次本处理器；但补发本身不依赖
			// 本地 PC——服务端按房间内存状态处理，白名单在 peer 建立后补应用
			// （handleRTCOffer → applyRoomWhisper）。早退会让 whisper 目标与
			// voice.state 与服务端长期不一致，所以只在拿到锁时才建 peer。
			if s.roomRebuildMu.TryLock() {
				s.setupPeer()
				s.roomRebuildMu.Unlock()
			} else {
				s.log.Printf("session: 入房 %s 时已有重建在途，跳过建 peer，仍补发语音状态与悄悄话目标", d.ChannelID)
			}
			s.syncVoiceStateAfterJoin()
		}
	})
	// channel.audio 音质热更新（#46）：只对当前房间生效；其他频道（例如管理员
	// 在别处改了配置）忽略，等入房时的 channel.joined 拿权威值。
	s.ws.On(signal.TypeChannelAudio, func(env signal.Envelope) {
		var d signal.ChannelAudioData
		if env.Decode(&d) != nil || d.ChannelID == "" {
			return
		}
		if room, _ := s.roomID.Load().(string); room != d.ChannelID {
			return
		}
		s.setChannelAudio(int(d.AudioBitrateKbps), int(d.AudioFrameMs))
	})
	// voice.whisper_ack：服务端确认目标集合已应用到本连接的 SFU peer（#116，
	// 协议 v4）。
	s.ws.On(signal.TypeVoiceWhisperAck, s.handleWhisperAck)
	s.ws.On(signal.TypeRTCOffer, func(env signal.Envelope) {
		var d signal.SDPData
		if env.Decode(&d) != nil {
			return
		}
		if p := s.peerRef.Load(); p != nil {
			_ = p.HandleRemoteSDP(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP})
		}
	})
	s.ws.On(signal.TypeRTCAnswer, func(env signal.Envelope) {
		var d signal.SDPData
		if env.Decode(&d) != nil {
			return
		}
		if p := s.peerRef.Load(); p != nil {
			_ = p.HandleRemoteSDP(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: d.SDP})
		}
	})
	s.ws.On(signal.TypeRTCIce, func(env signal.Envelope) {
		var d signal.ICEData
		if env.Decode(&d) != nil {
			return
		}
		if p := s.peerRef.Load(); p != nil {
			_ = p.HandleRemoteICE(iceToPion(d))
		}
	})
	s.ws.On(signal.TypeVoiceState, func(env signal.Envelope) {
		var d signal.VoiceStateData
		if env.Decode(&d) != nil || d.UserID == "" {
			return
		}
		s.states.Apply(d.UserID, d.Speaking, d.Mute, d.Deafen)
		s.mu.Lock()
		if v, ok := s.members[d.UserID]; ok {
			if d.Speaking != nil {
				v.Speaking = *d.Speaking
			}
			if d.Mute != nil {
				v.Mute = *d.Mute
			}
			if d.Deafen != nil {
				v.Deafen = *d.Deafen
			}
			s.members[d.UserID] = v
		}
		s.mu.Unlock()
		s.updateDucking()
	})
	// voice.priority 优先发言实时变更（#45）：频道覆盖变更后由服务端下发；
	// 角色/成员角色变更不实时广播，重进房由 channel.joined 刷新。旧房事件丢弃。
	s.ws.On(signal.TypeVoicePriority, func(env signal.Envelope) {
		var d signal.VoicePriorityData
		if env.Decode(&d) != nil || d.UserID == "" || d.ChannelID == "" {
			return
		}
		if room, _ := s.roomID.Load().(string); room != d.ChannelID {
			return
		}
		s.mu.Lock()
		v, ok := s.members[d.UserID]
		if ok {
			v.Priority = d.Priority
			s.members[d.UserID] = v
		}
		s.mu.Unlock()
		if !ok {
			return
		}
		if s.ev.OnPeerPriority != nil {
			s.ev.OnPeerPriority(d.UserID, d.Priority)
		}
		s.updateDucking()
	})
	// voice.speaking：服务端按 RFC 6464 电平统一判定的说话翻转（#33），含自己
	//（自己的声波球同源，v1「自己不亮」的问题随本消息消失）。mute/deafen 电平
	// 表达不了，仍走 voice.state。
	s.ws.On(signal.TypeVoiceSpeaking, func(env signal.Envelope) {
		var d signal.VoiceSpeakingData
		if env.Decode(&d) != nil || d.UserID == "" {
			return
		}
		s.mu.Lock()
		if v, ok := s.members[d.UserID]; ok {
			v.Speaking = d.Speaking
			s.members[d.UserID] = v
		}
		selfID := s.selfID
		s.mu.Unlock()
		if d.UserID == selfID {
			// 自己的 mute/deafen 以本地状态为准：StateTable 无 self 条目，
			// 用表值回填会把前端的闭麦显示冲掉。
			if s.ev.OnPeerSpeaking != nil {
				s.ev.OnPeerSpeaking(d.UserID, d.Speaking, s.muted.Load(), s.deafened.Load())
			}
			return
		}
		if s.states != nil {
			s.states.Apply(d.UserID, &d.Speaking, nil, nil)
		}
		s.updateDucking()
	})
	s.ws.On(signal.TypeChannelPeerLeft, func(env signal.Envelope) {
		var d signal.PeerLeftData
		if env.Decode(&d) != nil {
			return
		}
		if s.remoteMgr != nil {
			s.remoteMgr.RemovePeer(d.UserID)
		}
		if s.states != nil {
			s.states.Remove(d.UserID)
		}
		s.mu.Lock()
		delete(s.members, d.UserID)
		s.mu.Unlock()
		s.updateDucking()
		if s.ev.OnPeerLeave != nil {
			s.ev.OnPeerLeave(d.UserID)
		}
	})
	// voice.state_request：他人请求我闭麦/静音（v0.1.2，定向投递）。
	s.ws.On(signal.TypeVoiceStateRequest, func(env signal.Envelope) {
		var d signal.VoiceStateRequestData
		if env.Decode(&d) != nil {
			return
		}
		if s.ev.OnStateRequest != nil {
			s.ev.OnStateRequest(d.From.ID, d.From.Nickname, d.Mute, d.Deafen)
		}
	})
	// session.kicked：同类设备在其他设备登录，服务端踢本连接（契约 §0.5）。
	// 断开与清令牌在 app 层 OnKicked 完成，此处只上报原因。
	// 消息本身「尽力而为」（发送窗口内丢帧即无踪），WS close 4001 与握手
	// 401 的 session_replaced 头是兜底，见 wsCfg.OnKicked。
	s.ws.On(signal.TypeSessionKicked, func(env signal.Envelope) {
		var d signal.SessionKickedData
		reason := signal.KickReasonLoginElsewhere
		if env.Decode(&d) == nil && d.Reason != "" {
			reason = d.Reason
		}
		s.handleKicked(reason)
	})
	// session.voice_taken：同账号另一会话抢占语音（契约 §0.10）。服务端已把
	// 本端摘出成员表，本地按退房清理——但绝不发 channel.leave：那条 leave 按
	// uid 处理，会把抢占端一起带离房间。
	s.ws.On(signal.TypeSessionVoiceTaken, func(env signal.Envelope) {
		s.leaveRoomLocal()
		if s.ev.OnVoiceTaken != nil {
			s.ev.OnVoiceTaken()
		}
	})
	// presence.update：好友在线状态变化（#11/#92），原样转给 UI 层。
	s.ws.On(signal.TypePresenceUpdate, func(env signal.Envelope) {
		var d signal.PresenceUpdateData
		if env.Decode(&d) != nil || d.UserID == "" {
			return
		}
		if s.ev.OnPresenceUpdate != nil {
			s.ev.OnPresenceUpdate(d.UserID, d.Online, d.ChannelID, d.ServerID)
		}
	})
	// conversation.changed：私聊/群聊变更（#38 B3）。只带会话 ID 与动作，成员
	// 变化时新旧成员都会收到；删除/退出后 action=deleted，由 UI 从列表移除。
	// 跑在 ws 读循环，不得阻塞（REST 重拉由 UI 侧异步做）。
	s.ws.On(signal.TypeConversationChanged, func(env signal.Envelope) {
		var d signal.ConversationChangedData
		if env.Decode(&d) != nil || d.ConversationID == "" {
			return
		}
		if s.ev.OnConversationChanged != nil {
			s.ev.OnConversationChanged(d.ConversationID, d.Action)
		}
	})
	// chat.message：实时消息统一分发点（旧 ChatHandler / #39 ChatMessageHandler
	// 只存回调，不各自注册，避免两边同时设置时双投递）。
	s.ws.On(signal.TypeChatMessage, s.handleChatMessage)
	// message.edited：编辑广播（#39），按 message_id 原地替换正文与提及。
	// 跑在 ws 读循环，只转发，不得阻塞（UI 侧合并由前端状态做）。
	s.ws.On(signal.TypeMessageEdited, func(env signal.Envelope) {
		var d signal.MessageEditedData
		if env.Decode(&d) != nil || d.ChannelID == "" || d.MessageID == "" {
			return
		}
		if s.ev.OnMessageEdited != nil {
			s.ev.OnMessageEdited(d)
		}
	})
	// message.deleted：软删除广播（#39），UI 按 message_id 就地显示墓碑。
	s.ws.On(signal.TypeMessageDeleted, func(env signal.Envelope) {
		var d signal.MessageDeletedData
		if env.Decode(&d) != nil || d.ChannelID == "" || d.MessageID == "" {
			return
		}
		if s.ev.OnMessageDeleted != nil {
			s.ev.OnMessageDeleted(d)
		}
	})
	// channel.unread：按接收者精确计算的未读/提及计数（#40）。事件是覆盖语义，
	// 前端收到后整体替换该频道的计数；空 channel_id 的坏包不回调。跑在 ws 读
	// 循环，只转发，不得阻塞。
	s.ws.On(signal.TypeChannelUnread, func(env signal.Envelope) {
		var d signal.ChannelUnreadData
		if env.Decode(&d) != nil || d.ChannelID == "" {
			return
		}
		if s.ev.OnChannelUnread != nil {
			s.ev.OnChannelUnread(d)
		}
	})
	// server.shutdown：服务端优雅关闭（#21）。停掉 ws 自动重连并转给 UI 层提示。
	// Close 必须异步：同步调用会与读循环互等（Close 等 Run 退出、Run 等读循环返回）。
	s.ws.On(signal.TypeServerShutdown, func(env signal.Envelope) {
		var d signal.ServerShutdownData
		reason := "shutdown"
		if env.Decode(&d) == nil && d.Reason != "" {
			reason = d.Reason
		}
		if s.ev.OnServerShutdown != nil {
			s.ev.OnServerShutdown(reason)
		}
		go s.ws.Close()
	})
	// rtc.failed：SFU 因 ICE 失败移除本端（#19）。服务端已把本端摘出成员表并
	// 给他人广播 peer_left，本地销毁 PC 与远端链路后重发 channel.join 重建
	//（channel.joined 见 peerRef 为空会 setupPeer 建新 PC）。契约 0.2。
	s.ws.On(signal.TypeRTCFailed, func(env signal.Envelope) {
		var d signal.RTCFailedData
		if env.Decode(&d) != nil {
			return
		}
		s.rebuildAfterICEFail(d.ChannelID)
	})
	// 服务端 error 帧：先归因进房失败并回滚房间态（#91），其余再转聊天错误
	// 回调。必须内置在会话里——回滚不能依赖 app 层是否注入了 ErrorHandler。
	s.ws.On(signal.TypeError, s.handleErrorFrame)
}

// rebuildAfterICEFail ICE 失败重建入房（#19）：销毁本地 PC 与远端链路，重发
// channel.join 重建（读循环内禁网络 I/O，ICE 用缓存，与 OnConnect 重连同款）。
// 陈旧通知不折腾：不在该房（已手动离开/换房）忽略；本地 PC 已连通说明是
// 迟到的失败通知，再拆一次只会把健康链路打断。
func (s *Session) rebuildAfterICEFail(channelID string) {
	room, _ := s.roomID.Load().(string)
	if room == "" || (channelID != "" && channelID != room) {
		return
	}
	if p := s.peerRef.Load(); p != nil &&
		p.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return
	}
	// 已有重建在途（网络切换/并发的 rtc.failed）：让在途那次完成即可，
	// 读循环不等待（#49：网络恢复可能正在预取凭证）。
	if !s.roomRebuildMu.TryLock() {
		return
	}
	defer s.roomRebuildMu.Unlock()
	// 锁内重读：等锁期间可能已退房/换房。
	if room, _ = s.roomID.Load().(string); room == "" || (channelID != "" && channelID != room) {
		return
	}
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	if s.remoteMgr != nil {
		s.remoteMgr.RemoveAll()
	}
	if s.ws != nil {
		// 重建入房同样登记 pending：重建被拒（频道已删/无权限）时按进房失败
		// 回滚，而不是把幽灵房间留给下一次重连（#91）。
		seq := s.beginJoin(room)
		if err := s.ws.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: room, Password: s.roomPasswordCached()}); err != nil {
			s.abortJoinSend(seq, false)
			s.log.Printf("session: ICE 失败重建 channel.join 发送失败: %v", err)
		}
	}
	s.log.Printf("session: ICE 失败重建入房 %s（#19）", room)
	if s.ev.OnRTCFailed != nil {
		s.ev.OnRTCFailed(room)
	}
}

// leaveRoomLocal 本地退房（不发 channel.leave）。仅用于服务端已单方面把本端
// 摘出房间的路径（session.voice_taken），以及进房失败后的本地回滚：再发
// channel.leave 会让服务端按 uid 把抢占端也带离房间。成员表必须清——否则
// UI 拿着旧 members 显示「幽灵在房」。
func (s *Session) leaveRoomLocal() {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	// 退房后迟到的 error/joined 不得再回滚或恢复房间态，在途 join 一并作废。
	s.pendingJoins = nil
	s.roomID.Store("")
	s.roomPassword = "" // #37：退房即清密码（voice_taken/进房失败回滚走这里）
	s.teardownLocalRoomLocked()
}

// teardownLocalRoomLocked 清媒体链路、旧 PC、远端链路与成员表。调用方须持
// joinMu 并已定稿 roomID/pending（本函数不碰 join 状态）。
func (s *Session) teardownLocalRoomLocked() {
	s.stopMedia()
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	if s.remoteMgr != nil {
		s.remoteMgr.RemoveAll()
	}
	if s.mixer != nil {
		s.mixer.SetDeafen(false)
	}
	// #46：离开频道即取消码率上限、帧长回默认，恢复用户自己的设置。
	s.resetChannelAudio()
	s.clearRoomTables()
}

// clearRoomTables 清空成员表与语音状态表（媒体/peer 的清理由调用方负责）。
// 状态表只在 peer_left 删条目会让历史成员在同一会话内累积，且退房后旧成员的
// 说话态仍可能推 UI；这里换新 map 才能释放桶。
func (s *Session) clearRoomTables() {
	s.mu.Lock()
	s.members = make(map[string]PeerView)
	s.mu.Unlock()
	if s.states != nil {
		s.states.Clear()
	}
}

// handleKicked 上报被踢原因（session.kicked 消息、WS close 4001、握手 401 的
// session_replaced/session_replaced 头、Run 的 ErrAuthFatal 兜底，统一出口）。
// 同一会话只上报一次（#98）：不可恢复的鉴权失败可能先经 OnAuthFailed 回调、
// 再由 Run 退出兜底，重复事件会让 UI 重复弹横幅。
func (s *Session) handleKicked(reason string) {
	if !s.kicked.CompareAndSwap(false, true) {
		return
	}
	if s.ev.OnKicked != nil {
		s.ev.OnKicked(reason)
	}
}

// setupPeer 建立对 SFU 的单条 PeerConnection（初始 offer 一次，此后只应答）。
// offer 发送失败只记日志（读循环路径没有可回滚的更好选择，等下一次 joined）。
func (s *Session) setupPeer() { _ = s.setupPeerOffer() }

// setupPeerOffer 同 setupPeer，但把建 PC 与生成初始 offer 的错误返回给
// 调用方：网络变化恢复据此清掉半成品，交由 WS 重连兜底。信令发送失败在
// OnLocalSDP 回调里只能忽略——连接断开时 OnDisconnect 会清 peerRef。
func (s *Session) setupPeerOffer() error {
	if old := s.peerRef.Swap(nil); old != nil {
		_ = old.Close()
	}
	if s.remoteMgr == nil { // 音频降级路径不可能走到（JoinChannel 已拦截），防御
		return errors.New("remote manager unavailable")
	}
	s.remoteMgr.RemoveAll()
	streamID := s.name
	if s.selfID != "" {
		streamID = s.selfID
	}
	// 媒体降级时 pipe 为空：不挂电平扩展即可，不能解引用 nil（防御）。
	var levelFn func() uint8
	if s.pipe != nil {
		levelFn = s.pipe.AudioLevel
	}
	peer, err := rtc.NewPeer(rtc.PeerConfig{
		StreamID: streamID,
		// 只读预取缓存，不做网络 I/O：本函数跑在 WS 读循环的 channel.joined
		// 处理器里，同步拉凭证会把信令收发停摆到撞心跳预算（#13）。
		ICEServers:      s.cachedICE(),
		IncludeLoopback: IsLoopbackURL(s.api.Base()), // 环回候选仅本机/内网 SFU 需要
		// RFC 6464 电平源（#33）：管线 DSP 已算好的帧电平，SendOpus 每包读一次。
		LevelFn: levelFn,
		Callbacks: rtc.PeerCallbacks{
			OnLocalSDP: func(sdp webrtc.SessionDescription) {
				msgType := signal.TypeRTCOffer
				if sdp.Type == webrtc.SDPTypeAnswer {
					msgType = signal.TypeRTCAnswer
				}
				_ = s.ws.Send(msgType, signal.SDPData{SDP: sdp.SDP})
			},
			OnLocalICE: func(c webrtc.ICECandidateInit) {
				_ = s.ws.Send(signal.TypeRTCIce, iceFromPion(c))
			},
			OnTrack: s.remoteMgr.HandleTrack,
			OnConnectionStateChange: func(st webrtc.PeerConnectionState) {
				if s.ev.OnConnection != nil {
					s.ev.OnConnection("rtc:" + st.String())
				}
			},
		},
		Logger: s.log,
	})
	if err != nil {
		s.log.Printf("new peer: %v", err)
		return err
	}
	// 新 peer 默认不静音：必须同步本地静音态，否则重连后出现
	//「UI 显示已闭麦但声音照传」。deafen 是混音器总线层的口，无 peer 级对应。
	peer.SetMuted(s.muted.Load())
	// #46：上行 RTP 时戳步进必须匹配频道帧长（10ms=480/20ms=960）。此时
	// channel.joined 已应用频道配置；下行远端链路也已同步同一帧长。
	if err := peer.SetFrameMS(s.currentFrameMS()); err != nil {
		s.log.Printf("peer frame size: %v", err)
	}
	s.peerRef.Store(peer)
	if err := peer.Offer(); err != nil {
		s.log.Printf("offer: %v", err)
		return err
	}
	return nil
}

// JoinChannel 加入语音频道（不带密码；带密码用 JoinChannelWithPassword）。
// 发送成功只代表请求已发出——服务端是否接纳是异步回执（channel.joined /
// error 帧），见 beginJoin/handleErrorFrame（#91）。
func (s *Session) JoinChannel(channelID string) error {
	return s.JoinChannelWithPassword(channelID, "")
}

// JoinChannelWithPassword 带密码入房（#37）。password 每次 join 覆盖缓存（含空串），
// 重连/ICE 重建的 channel.join 从缓存取——缓存不落盘、不日志。
func (s *Session) JoinChannelWithPassword(channelID, password string) error {
	if s.ws == nil {
		return fmt.Errorf("session not connected")
	}
	if s.voiceUnavailable {
		return fmt.Errorf("语音不可用: %w", s.voiceErr)
	}
	// 预取 ICE 凭证再进房：setupPeer 跑在 WS 读循环上，不能做网络 I/O（#13）。
	// 这里是 Wails 绑定 goroutine，阻塞可接受；失败留空缓存，降级空 ICE 列表。
	s.prefetchICE()
	s.setRoomPassword(password)
	seq := s.beginJoin(channelID)
	if err := s.ws.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: channelID, Password: password}); err != nil {
		// roomID 先存是为断线重连恢复；发送失败必须回滚——否则重连后
		// OnConnect 重发 join 造成「幽灵入房」。
		s.abortJoinSend(seq, true)
		return fmt.Errorf("join channel: %w", err)
	}
	return nil
}

// setRoomPassword 覆写入房密码缓存（#37）：仅用户主动 join 时调用；
// 重连/ICE 重建只读不写。
func (s *Session) setRoomPassword(pw string) {
	s.joinMu.Lock()
	s.roomPassword = pw
	s.joinMu.Unlock()
}

// roomPasswordCached 读入房密码缓存（#37）：重连/ICE 重建的 channel.join 用。
func (s *Session) roomPasswordCached() string {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	return s.roomPassword
}

// beginJoin 登记一次进房意图并乐观写 roomID（断线重连据此恢复）。返回的
// seq 供发送失败时精确摘除本条；并发 join 按登记顺序排队，服务端在同一
// 连接上按序处理，error 帧据此归属（#91）。
func (s *Session) beginJoin(channelID string) uint64 {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	s.joinSeq++
	s.pendingJoins = append(s.pendingJoins, pendingJoin{channelID: channelID, seq: s.joinSeq})
	if n := len(s.pendingJoins); n > maxPendingJoins {
		// 只保留最近的意图：超限说明调用方在异常连点，旧请求的结算已无意义。
		s.pendingJoins = append([]pendingJoin(nil), s.pendingJoins[n-maxPendingJoins:]...)
	}
	s.roomID.Store(channelID)
	return s.joinSeq
}

// abortJoinSend 发送失败：摘掉本次 join。rollbackRoom=true（用户主动 JoinChannel）
// 且 roomID 仍是该频道时回滚为空；重连/ICE 重建的重发传 false——保留 roomID
// 等下次重连再试是原有语义。
func (s *Session) abortJoinSend(seq uint64, rollbackRoom bool) {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	idx := -1
	for i := range s.pendingJoins {
		if s.pendingJoins[i].seq == seq {
			idx = i
			break
		}
	}
	if idx < 0 {
		return // 已被 channel.joined / error 结算，不能再动房间态
	}
	ch := s.pendingJoins[idx].channelID
	s.pendingJoins = append(s.pendingJoins[:idx], s.pendingJoins[idx+1:]...)
	if !rollbackRoom {
		return
	}
	if cur, _ := s.roomID.Load().(string); cur == ch {
		// 不恢复到更早的 pending：发送失败时那些 join 也未必成功，等回执确认。
		s.roomID.Store("")
		s.roomPassword = "" // #37：发送失败回滚即清密码
	}
}

// confirmJoin channel.joined 回执：匹配到本端发出且未结算的请求时摘除并返回
// true；广播帧（成员变化重发全量）与迟到帧不改变状态，返回 false——调用方
// 据此区分「我方 join 的确认」与「房内广播」。
func (s *Session) confirmJoin(channelID string) bool {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	for i := range s.pendingJoins {
		if s.pendingJoins[i].channelID != channelID {
			continue
		}
		s.pendingJoins = append(s.pendingJoins[:i], s.pendingJoins[i+1:]...)
		// 发送失败曾清空 roomID、而更早的 join 实际成功：以服务端回执为准恢复。
		// roomID 非空（已有更新的 join 意图）时不得覆盖。
		if cur, _ := s.roomID.Load().(string); cur == "" {
			s.roomID.Store(channelID)
		}
		return true
	}
	return false
}

// settlePendingJoin 结算最早一次未完成的 join（服务端同一连接按序处理上行，
// error 帧按 FIFO 归属）。rollback=false 用于 already_in_channel——服务端确认
// 本连接已在该房，只清 pending，不清房间态。返回的 rolledBack 表示确实回滚了
// 当前房间，调用方据此决定是否上报 OnJoinFailed。
func (s *Session) settlePendingJoin(rollback bool) (channelID string, consumed, rolledBack bool) {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	if len(s.pendingJoins) == 0 {
		return "", false, false
	}
	head := s.pendingJoins[0]
	s.pendingJoins = s.pendingJoins[1:]
	if !rollback {
		return head.channelID, true, false
	}
	// 快速切房：失败的是旧 join，当前 roomID 已指向新房，绝不能回滚新房。
	if cur, _ := s.roomID.Load().(string); cur != head.channelID {
		return head.channelID, true, false
	}
	s.roomID.Store("")
	s.roomPassword = "" // #37：进房失败回滚即清密码
	s.teardownLocalRoomLocked()
	return head.channelID, true, true
}

// LeaveChannel 离开当前语音频道。
func (s *Session) LeaveChannel() error {
	s.joinMu.Lock()
	room, _ := s.roomID.Load().(string)
	// 主动离开后迟到的 join 回执/错误不得再改状态：在途意图全部作废。
	s.pendingJoins = nil
	s.roomID.Store("")
	s.roomPassword = "" // #37：主动退房清密码
	// 与 voice_taken 共用同一条本地拆房路径：媒体、peer、远端链路、成员表与
	// 状态表一起清；放在 joinMu 内，避免并发 JoinChannel 装上新 peer 后被误关。
	s.teardownLocalRoomLocked()
	s.joinMu.Unlock()
	if room == "" || s.ws == nil {
		return nil
	}
	// 先拆本地再发 leave：发送失败也不留下幽灵成员/旧说话态。
	return s.ws.Send(signal.TypeChannelLeave, signal.ChannelLeaveData{ChannelID: room})
}

// SetSendGate 注入按键说话发送门（传 nil 清除）。管线已建立时立即生效，
// 未建立（未 Connect 或音频降级）时先存住，Connect 建管线后自动补上。
// 跨 goroutine 安全：内部 atomic。
func (s *Session) SetSendGate(g *hotkey.Gate) {
	s.sendGate.Store(g)
	if s.pipe != nil {
		s.pipe.SetSendGate(g)
	}
}

// SendGate 返回当前发送门（未注入为 nil）。
func (s *Session) SendGate() *hotkey.Gate { return s.sendGate.Load() }

// SetMute 本地静音（停止上行发包 + 隐藏波形）。
func (s *Session) SetMute(muted bool) {
	s.muted.Store(muted)
	if p := s.peerRef.Load(); p != nil {
		p.SetMuted(muted)
	}
	if s.ws != nil {
		_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{Mute: signal.Bool(muted)})
	}
}

// SetDeafen 耳聋（不听任何人；实现为混音器总输出静音）。
func (s *Session) SetDeafen(deafened bool) {
	s.deafened.Store(deafened)
	if s.mixer != nil {
		s.mixer.SetDeafen(deafened)
	}
	if s.ws != nil {
		_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{Deafen: signal.Bool(deafened)})
	}
}

// SetPeerVolume 调整某远端成员音量（0.0–2.0）。与优先发言 ducking 独立：
// duck 乘数只作用于混音 Tick，不改写这里的用户设置（#45）。
func (s *Session) SetPeerVolume(userID string, gain float64) {
	if s.mixer != nil {
		if in := s.mixer.Peer(userID); in != nil {
			in.SetGain(gain)
		}
	}
}

// SetWhisperTargets 更新同频道悄悄话目标（#45）。空列表 = 取消，仅协议 ≥3
// 时真正下发（v2 服务端不认该消息）。非阻塞：只保存最新值并敲一下容量 1 的
// 唤醒通道，网络发送由 Connect 里的专用 goroutine 完成——不得在 Wails 绑定
// 线程或音频回调里同步 ws.Send。
//
// 这是 v3 兼容路径（seq=0，服务端不回 ack）；协议 v4 需要确认目标生效后才
// 开麦时用 SendWhisperTargets。
func (s *Session) SetWhisperTargets(targets []string) {
	s.whisperMu.Lock()
	s.queueWhisperLocked(cleanWhisperTargets(targets), 0)
	s.whisperMu.Unlock()
	s.wakeWhisper()
}

// SendWhisperTargets 更新目标并返回本次请求序号（#116，协议 v4）。
// 协议 <3 或信令未连接时返回 ok=false 且不排队——调用方据此提示，而不是
// 空等一个永远不会到来的 ack。目标为空仍是取消；是否真正下发沿用与
// SetWhisperTargets 相同的去重语义（服务端已可能为空时补发一条兜底取消）。
// 非阻塞：只保存最新值与序号并唤醒专用发送 goroutine。
func (s *Session) SendWhisperTargets(targets []string) (int64, bool) {
	if s.protoVer.Load() < 3 || s.ws == nil {
		return 0, false
	}
	s.whisperMu.Lock()
	s.whisperSeq++
	seq := s.whisperSeq
	s.queueWhisperLocked(cleanWhisperTargets(targets), seq)
	s.whisperMu.Unlock()
	s.wakeWhisper()
	return seq, true
}

// queueWhisperLocked 写入最新目标与待发序号（调用方持 whisperMu）。空目标
// 只在之前确实有目标时才需要显式取消；重复的「本就为空」不下发（服务端残留
// 由入房同步兜底）。
func (s *Session) queueWhisperLocked(cp []string, seq int64) {
	oldHas := len(s.whisperWant) > 0
	s.whisperWant = cp
	if len(cp) > 0 {
		s.whisperNeedSend = true
		s.whisperMaybe = true
	} else if oldHas {
		s.whisperNeedSend = true
	}
	if s.whisperNeedSend {
		// 到达这里的一定是新的下发意图（用户改目标 / 入房同步），重试预算从零
		// 开始。重试自身的置脏走 onWhisperSendFail，不经过本函数（#117）。
		s.whisperFails = 0
		s.whisperSendSeq = seq
	}
}

// cleanWhisperTargets 过滤空 ID 并复制一份（切片一经发布不再原地修改）。
func cleanWhisperTargets(targets []string) []string {
	cp := make([]string, 0, len(targets))
	for _, uid := range targets {
		if uid != "" {
			cp = append(cp, uid)
		}
	}
	return cp
}

// ClearWhisperTargets 退房/切房时清空会话缓存的悄悄话目标，且不下发任何
// 信令（#45）：旧房间的 Peer 会随离房销毁，此刻发取消可能撞上 #91 的
// pending join 错误归属窗口。新房间入房时 syncWhisperOnJoin 会补一条空数组，
// 清掉服务端可能残留的旧目标。
func (s *Session) ClearWhisperTargets() {
	s.whisperMu.Lock()
	s.whisperWant = nil
	s.whisperNeedSend = false
	s.whisperSendSeq = 0
	// 退房会销毁 peer，在途重试没有意义：停掉计时器、清空重试预算（#117）。
	if s.whisperRetryTimer != nil {
		s.whisperRetryTimer.Stop()
		s.whisperRetryTimer = nil
	}
	s.whisperFails = 0
	s.whisperMu.Unlock()
}

// wakeWhisper 合并一次下发唤醒（容量 1；发送 goroutine 总是读最新值）。
func (s *Session) wakeWhisper() {
	select {
	case s.whisperWake <- struct{}{}:
	default:
	}
}

// 悄悄话下发重试参数（#117）。发送失败时服务端仍按旧目标集合转发，正是
// 「以为在悄悄话、其实全房能听到」（或反之）的来源，必须重试到成功或放弃：
// 立即重试会与断线重连的抖动抢同一段网络，故指数退避。
const (
	whisperRetryBase    = 100 * time.Millisecond
	whisperRetryMax     = 2 * time.Second
	whisperSendMaxTries = 6
)

// whisperRetryDelay 第 n 次失败后的等待时长：100ms 起翻倍，封顶 2s。
func whisperRetryDelay(fails int) time.Duration {
	d := whisperRetryBase
	for i := 1; i < fails && d < whisperRetryMax; i++ {
		d *= 2
	}
	if d > whisperRetryMax {
		d = whisperRetryMax
	}
	return d
}

// whisperLoop 悄悄话目标发送循环（#45，Connect 启动；测试可直接跑）。
// 每次唤醒读最新目标；协议 <3 时静默丢弃（v2 服务端不认 voice.whisper）。
// 发送失败重新置脏并按指数退避排一次唤醒重试（#117）：失败期间服务端可能
// 仍按旧目标集合转发，不重试就留下「本地以为在悄悄话、服务端按全房发」的
// 分歧。重试用尽仍失败则放弃本次下发——保留 whisperMaybe（服务端状态未知），
// 由下一次目标变化或入房同步兜底，不能让一条永远发不出去的消息把 goroutine
// 钉死。重试同样走本 goroutine 的 wake 队列，不占用音频回调线程。
func (s *Session) whisperLoop(ctx context.Context) {
	defer s.stopWhisperRetry()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.whisperWake:
			targets, seq, send := s.pendingWhisper()
			if !send || s.protoVer.Load() < 3 || s.ws == nil {
				continue
			}
			if err := s.ws.Send(signal.TypeVoiceWhisper, signal.VoiceWhisperData{Targets: targets, Seq: seq}); err != nil {
				s.onWhisperSendFail(seq, err)
			} else {
				s.onWhisperSendOK(targets)
			}
		}
	}
}

// onWhisperSendOK 发送成功：清重试计数与在途计时器，并更新「服务端可能有非空
// 目标」。失败时不能更新该标记——时间线未知，保守保留原值等入房同步兜底。
func (s *Session) onWhisperSendOK(targets []string) {
	s.stopWhisperRetry()
	s.whisperMu.Lock()
	s.whisperFails = 0
	s.whisperMaybe = len(targets) > 0
	s.whisperMu.Unlock()
}

// onWhisperSendFail 发送失败：未达上限则重新置脏并排一次退避唤醒；达上限放弃
// 本次（脏标记保持已消费，避免同一条消息无限循环），交给下次目标变化/入房同步。
func (s *Session) onWhisperSendFail(seq int64, err error) {
	s.whisperMu.Lock()
	s.whisperFails++
	fails := s.whisperFails
	if fails >= whisperSendMaxTries {
		s.whisperMu.Unlock()
		s.log.Printf("session: 悄悄话目标下发重试 %d 次仍失败（%v），交给下次目标变化/入房同步", fails, err)
		return
	}
	// 置脏让唤醒后重发同一版本；期间用户再改目标时 queueWhisperLocked 会覆盖
	// 成更新的版本，本次重试自然作废（唤醒总是读最新值）。
	s.whisperNeedSend = true
	s.whisperSendSeq = seq
	delay := whisperRetryDelay(fails)
	if s.whisperRetryTimer != nil {
		s.whisperRetryTimer.Stop()
	}
	s.whisperRetryTimer = time.AfterFunc(delay, s.wakeWhisper)
	s.whisperMu.Unlock()
	s.log.Printf("session: 悄悄话目标下发失败（第 %d 次，%v），%s 后重试", fails, err, delay)
}

// stopWhisperRetry 停掉在途重试计时器（成功、会话关闭时）。调用方不得持 whisperMu。
func (s *Session) stopWhisperRetry() {
	s.whisperMu.Lock()
	if s.whisperRetryTimer != nil {
		s.whisperRetryTimer.Stop()
		s.whisperRetryTimer = nil
	}
	s.whisperMu.Unlock()
}

// pendingWhisper 取当前待下发快照并清掉脏标记。空目标也要返回（显式取消），
// 由 send 区分「无需发送」与「发送空数组」；seq 是该版本的请求序号（0 = v3
// 兼容路径）。切片一经发布不再原地修改。
func (s *Session) pendingWhisper() ([]string, int64, bool) {
	s.whisperMu.Lock()
	want := s.whisperWant
	seq := s.whisperSendSeq
	send := s.whisperNeedSend
	s.whisperNeedSend = false
	s.whisperSendSeq = 0
	s.whisperMu.Unlock()
	if want == nil {
		want = []string{} // voice.whisper 的 targets 是 required 数组，不能下发 null
	}
	return want, seq, send
}

// syncVoiceStateAfterJoin 入房/重连后的状态补发（#117）：voice.state 现值 +
// 悄悄话目标同步。与建 peer 解耦——两者都只依赖服务端房间内存状态，不依赖
// 本地 PC（白名单在 peer 建立后由 handleRTCOffer → applyRoomWhisper 补应用），
// 所以即使重建在途（拿不到 roomRebuildMu）也必须执行；否则 UI 显示已闭麦而
// 服务端照传、或悄悄话目标停在旧集合。
// 跑在 ws 读循环上：只投递两条信令，不做额外等待。
func (s *Session) syncVoiceStateAfterJoin() {
	// 补发完整语音状态（muted+deafened 现值）：重连/重入房后服务端内存里的
	// 旧状态可能已丢，peer 又是新建（默认不静音）——不发则「UI 显示已闭麦但声音照传」。
	if s.ws != nil {
		_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{
			Mute:   signal.Bool(s.muted.Load()),
			Deafen: signal.Bool(s.deafened.Load()),
		})
	}
	// 重连/重入房后 SFU peer 是新的：把当前目标同步过去。即使 want 为空
	// 也发一条空数组，清掉服务端可能残留的旧目标（切房前在途发送的兜底）。
	s.syncWhisperOnJoin()
}

// syncWhisperOnJoin 入房/重连后把当前目标同步到新建的 SFU peer（#45）。
// want 为空但服务端可能残留旧目标（切房前在途发送）时强制下发一次空数组；
// 从未用过悄悄话则不发，避免每次入房都产生无意义信令。同步走 seq=0 的 v3
// 语义：入房重建不是一次用户请求，无需 voice.whisper_ack。
func (s *Session) syncWhisperOnJoin() {
	s.whisperMu.Lock()
	if s.whisperNeedSend && s.whisperSendSeq != 0 {
		// 队列里已有一条带 seq 的显式请求（用户按住悄悄话时恰好切房/重连）：
		// 服务端会在 peer 建立后补应用并回 ack，不能降级成 seq=0 的同步消息，
		// 否则 App 永远等不到确认，只能按超时关麦。
		s.whisperMu.Unlock()
		s.wakeWhisper()
		return
	}
	need := len(s.whisperWant) > 0 || s.whisperMaybe
	s.whisperNeedSend = need
	s.whisperSendSeq = 0
	if need {
		// 入房重建是一次新的下发意图（新 peer 需要重新应用白名单），重试预算
		// 从零开始（#117）。
		s.whisperFails = 0
	}
	s.whisperMu.Unlock()
	if need {
		s.wakeWhisper()
	}
}

// handleWhisperAck 分发 voice.whisper_ack（#116，协议 v4）。ack 不带频道 ID，
// 无法按频道比对：退房后到达的旧 ack 由「不在房即丢弃」+ App 侧按在途 seq
// 匹配（clearWhisper 会清零 seq）共同排除。跑在 ws 读循环 goroutine。
func (s *Session) handleWhisperAck(env signal.Envelope) {
	var d signal.VoiceWhisperAckData
	if env.Decode(&d) != nil || d.Seq <= 0 {
		return
	}
	if !s.InRoom() {
		return
	}
	if s.ev.OnWhisperAck != nil {
		s.ev.OnWhisperAck(d.Seq, d.Active)
	}
}

// updateDucking 重算各远端 peer 的优先发言 ducking（#45）：任一远端优先发言者
// 正在说话时，其他远端 peer 压低；优先发言者本人不压，自己说话不触发（本地
// 混音不含自己）。只在说话/优先状态翻转时调用，不在 Tick 热路径。
func (s *Session) updateDucking() {
	if s.mixer == nil {
		return
	}
	s.mu.Lock()
	prioritySpeaking := false
	ids := make([]string, 0, len(s.members))
	priority := make(map[string]bool, len(s.members))
	for uid, m := range s.members {
		if uid == s.selfID {
			continue
		}
		ids = append(ids, uid)
		if m.Priority {
			priority[uid] = true
			if m.Speaking {
				prioritySpeaking = true
			}
		}
	}
	s.mu.Unlock()
	for _, uid := range ids {
		if in := s.mixer.Peer(uid); in != nil {
			in.SetDucked(prioritySpeaking && !priority[uid])
		}
	}
}

// RestartDevices 热切换采集/播放设备：管线设备流重启，编解码/混音状态保留。
func (s *Session) RestartDevices(captureID, playbackID string) error {
	if s.pipe == nil {
		return errors.New("no pipeline")
	}
	return s.pipe.RestartDevices(captureID, playbackID)
}

// effectiveBitrateKbps 频道上限与用户设置取小（#46）：上限 0 = 跟随用户设置。
// 纯函数便于单测；用户设置已被 ResolveOpusConfig 兜底为正数。
func effectiveBitrateKbps(user, cap int) int {
	if cap > 0 && cap < user {
		return cap
	}
	return user
}

// normalizeChannelFrameMS 频道下发的帧长归一：只认 10，其余（含 0/20/非法值）
// 一律默认 20ms——旧服务端缺省字段与显式默认走同一条路径。
func normalizeChannelFrameMS(ms int) int {
	if ms == audio.FrameMS10 {
		return audio.FrameMS10
	}
	return audio.FrameMS20
}

// setChannelAudio 应用当前频道音质配置（#46）。只改码率/帧长，不碰用户其他
// 音频设置；离开频道时用 resetChannelAudio 恢复用户码率与 20ms 帧长。
func (s *Session) setChannelAudio(bitrateKbps, frameMS int) {
	s.audioCfgMu.Lock()
	defer s.audioCfgMu.Unlock()
	s.chanBitrateKbps = bitrateKbps
	s.chanFrameMS = normalizeChannelFrameMS(frameMS)
	if err := s.applyChannelAudioLocked(); err != nil {
		s.log.Printf("session: 应用频道音质失败: %v", err)
	}
}

// resetChannelAudio 离开房间后恢复用户设置（码率上限取消、帧长回 20ms）。
func (s *Session) resetChannelAudio() {
	s.audioCfgMu.Lock()
	defer s.audioCfgMu.Unlock()
	s.chanBitrateKbps = 0
	s.chanFrameMS = audio.FrameMS20
	if err := s.applyChannelAudioLocked(); err != nil {
		s.log.Printf("session: 恢复用户音质失败: %v", err)
	}
}

// currentFrameMS 当前生效帧长（建 peer 前读取）。
func (s *Session) currentFrameMS() int {
	s.audioCfgMu.Lock()
	defer s.audioCfgMu.Unlock()
	return normalizeChannelFrameMS(s.chanFrameMS)
}

// applyChannelAudioLocked 把生效码率/帧长刷到管线、上行 peer 与下行远端链路。
// 调用方须持 audioCfgMu；pipe 为 nil（音频降级）时只需维护状态。
func (s *Session) applyChannelAudioLocked() error {
	if s.pipe == nil {
		return nil
	}
	var firstErr error
	if err := s.pipe.SetOpusBitrate(effectiveBitrateKbps(s.userBitrateKbps, s.chanBitrateKbps)); err != nil {
		firstErr = fmt.Errorf("bitrate: %w", err)
	}
	frameMS := normalizeChannelFrameMS(s.chanFrameMS)
	if err := s.pipe.SetFrameMS(frameMS); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("frame: %w", err)
	}
	if p := s.peerRef.Load(); p != nil {
		if err := p.SetFrameMS(frameMS); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("peer frame: %w", err)
		}
	}
	if s.remoteMgr != nil {
		if err := s.remoteMgr.SetFrameMS(frameMS); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remote frame: %w", err)
		}
	}
	return firstErr
}

// ApplyAudio 把运行时音频设置热应用到媒体管线（通话中可调，未 Connect 时空操作）。
// Opus 参数经 ResolveOpusConfig 回落默认；逐项应用，返回首个错误（不中断）。
// v0.1.2 扩展：复杂度/降噪强度/AGC/扬声器总音量（mixer master gain）。
// v1.1 扩展：前置增益（原子热生效）/输入声道模式（热重启采集流）。
// #46：用户码率设置只存原始值，实际生效值再与当前频道上限取小；频道帧长不受
// 用户设置影响。
func (s *Session) ApplyAudio(rs audio.RuntimeSettings) error {
	if s.pipe == nil {
		return nil
	}
	rs.Opus = audio.ResolveOpusConfig(rs.Opus)
	s.audioCfgMu.Lock()
	s.userBitrateKbps = rs.Opus.BitrateKbps
	s.audioCfgMu.Unlock()
	steps := []struct {
		name string
		fn   func() error
	}{
		{"vbr", func() error { return s.pipe.SetOpusVBR(*rs.Opus.VBR) }},
		{"dtx", func() error { return s.pipe.SetOpusDTX(*rs.Opus.DTX) }},
		{"fec", func() error { return s.pipe.SetOpusFEC(*rs.Opus.FEC) }},
		{"dred", func() error { return s.pipe.SetOpusDRED(*rs.Opus.DRED) }},
		{"packet_loss", func() error { return s.pipe.SetOpusPacketLoss(rs.Opus.PacketLoss) }},
		{"pre_gain", func() error { return s.pipe.SetPreGainDB(rs.PreGainDB) }},
		{"input_mode", func() error { return s.pipe.SetInputMode(rs.InputMode) }},
	}
	if rs.Complexity > 0 {
		steps = append(steps, struct {
			name string
			fn   func() error
		}{"complexity", func() error { return s.pipe.SetComplexity(rs.Complexity) }})
	}
	var firstErr error
	for _, st := range steps {
		if err := st.fn(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("audio %s: %w", st.name, err)
		}
	}
	s.pipe.SetDenoise(rs.Denoise)
	if err := s.pipe.SetDenoiseStrength(rs.DenoiseStrength); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("audio denoise_strength: %w", err)
	}
	s.pipe.SetAGC(rs.AGC)
	s.pipe.SetGain(rs.Gain)
	if err := s.pipe.SetEnableAPM(rs.EnableAPM); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("audio enable_apm: %w", err)
	}
	if s.mixer != nil {
		s.mixer.SetMasterGain(rs.PlaybackGain)
	}
	s.pipe.SetVADSensitivity(rs.VADSens)
	// 码率与帧长统一走频道合并逻辑：用户调码率时若在受上限约束的房内，
	// 实际值不得越过频道上限。
	s.audioCfgMu.Lock()
	if err := s.applyChannelAudioLocked(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("audio channel: %w", err)
	}
	s.audioCfgMu.Unlock()
	return firstErr
}

// APMActive 报告当前媒体管线是否启用了 WebRTC APM（Beta 设置回滚/诊断）。
func (s *Session) APMActive() bool { return s.pipe != nil && s.pipe.APMActive() }

// Muted / Deafened 本地状态查询。
func (s *Session) Muted() bool    { return s.muted.Load() }
func (s *Session) Deafened() bool { return s.deafened.Load() }

// Members 房间成员快照。
func (s *Session) Members() []PeerView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerView, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, m)
	}
	return out
}

// Stats 统计快照（JSON 给 UI）。
func (s *Session) Stats() string {
	vs := VoiceStats{}
	var ps audio.StatsSnapshot
	havePipe := s.pipe != nil
	if havePipe {
		ps = s.pipe.Stats()
		vs.PlaybackBufferedMS = int(s.pipe.PlaybackBuffered().Milliseconds())
		vs.PlaybackUnderruns = ps.PlaybackUnderruns
		vs.PlaybackDropped = ps.PlaybackDropped
	}
	// #49 B：一次 GetStats 同时取 RTT 与全部入站 RTP 流。2s 轮询下重复
	// 调 GetStats 会把整张统计表拷贝两次，且入站流按 SSRC 与远端成员对账。
	var snap rtc.LinkSnapshot
	if p := s.peerRef.Load(); p != nil {
		snap = p.LinkSnapshot()
		vs.RtpSent = p.Stats.PacketsSent.Load()
	}
	inbound := inboundBySSRC(snap.Inbound)
	if s.remoteMgr != nil {
		vs.PeerIDs = s.remoteMgr.PeerIDs()
		frameMS := s.remoteMgr.FrameMS()
		// 下行链路诊断（#95/#49）：app 层每 2s 入房期间调用，快照读取。
		for _, id := range vs.PeerIDs {
			ls, ok := s.remoteMgr.LinkStats(id)
			if !ok {
				continue
			}
			diag := peerLinkDiag(id, ls, frameMS, inbound)
			vs.Peers = append(vs.Peers, diag)
			// 抖动/缓冲取最差一路：用户关心的是哪一路在拖后腿，平均后会
			// 把明显异常的一路稀释成「看起来还行」。丢包率则按总包数聚合，
			// 与 RTPReceived/RTPLost 两个计数保持自洽。
			if diag.RTPJitterMS > vs.RTPJitterMS {
				vs.RTPJitterMS = diag.RTPJitterMS
			}
			if diag.BufferMS > vs.JitterBufferMS {
				vs.JitterBufferMS = diag.BufferMS
			}
			if diag.TargetMS > vs.JitterTargetMS {
				vs.JitterTargetMS = diag.TargetMS
			}
			vs.RTPReceived += diag.RTPReceived
			vs.RTPLost += diag.RTPLost
		}
		vs.LossPct = lossPercent(vs.RTPReceived, vs.RTPLost)
	}
	if snap.HasRTT {
		// 延迟优先取 WebRTC 选中 ICE pair 的真实 RTT（问题：此前恒 0ms——
		// 管线回环估计只在 Loopback 模式有样本，真实通话不产样本）。
		// 用 ok 作哨兵而非值——RTT 真实但 <1ms 时会被错误覆盖（S1）。
		vs.LatencyMS = int(snap.RTT.Milliseconds())
	} else if havePipe {
		// 无 RTT 样本时回落管线回环估计（本机联调场景）。
		vs.EncPackets = ps.PacketsEncoded
		vs.LatencyMS = int(ps.LatencyAvgUS / 1000)
	}
	s.mu.Lock()
	vs.Members = len(s.members)
	s.mu.Unlock()
	b, _ := json.Marshal(vs)
	return string(b)
}

// inboundBySSRC 按 SSRC 索引 PeerConnection 的入站音频流，供 per-peer 归账。
func inboundBySSRC(streams []rtc.InboundStreamStats) map[uint32]rtc.InboundStreamStats {
	if len(streams) == 0 {
		return nil
	}
	m := make(map[uint32]rtc.InboundStreamStats, len(streams))
	for _, st := range streams {
		m[st.SSRC] = st
	}
	return m
}

// peerLinkDiag 组装单路诊断：抖动缓冲快照 + 同 SSRC 的入站 RTP 统计。
// 入站流尚未出现（对端未发过包）时只输出缓冲侧字段。
func peerLinkDiag(id string, ls audio.RemoteLinkStats, frameMS int, inbound map[uint32]rtc.InboundStreamStats) PeerLinkDiag {
	if frameMS <= 0 {
		frameMS = rtc.JitterFrameMS
	}
	diag := PeerLinkDiag{
		PeerID:                id,
		JitterMs:              ls.Jitter.JitterMs,
		JitterTargetPackets:   ls.Jitter.TargetPackets,
		JitterBufferedPackets: ls.Jitter.Buffered,
		JitterLost:            ls.Jitter.Lost,
		RingSamples:           ls.RingAvailable,
		PLCFrames:             ls.PLCFrames,
		FECFrames:             ls.FECFrames,
		DREDFrames:            ls.DREDFrames,
		UnderrunFrames:        ls.UnderrunFrames,
		DroppedSamples:        ls.DroppedSamples,
		BufferMS:              ls.Jitter.Buffered * frameMS,
		TargetMS:              ls.Jitter.TargetPackets * frameMS,
	}
	if st, ok := inbound[ls.SSRC]; ok {
		diag.LossPct = lossPercent(st.PacketsReceived, st.PacketsLost)
		diag.RTPJitterMS = float64(st.Jitter.Microseconds()) / 1000
		diag.RTPReceived = st.PacketsReceived
		diag.RTPLost = st.PacketsLost
	}
	return diag
}

// lossPercent 入站丢包率（%）。PacketsLost 在重复包多于丢失包时可为负，
// 统一夹到 0..100；还没有任何包时返回 0。
func lossPercent(received uint32, lost int32) float64 {
	lost64 := int64(lost)
	if lost64 < 0 {
		lost64 = 0
	}
	total := int64(received) + lost64
	if total <= 0 {
		return 0
	}
	pct := float64(lost64) * 100 / float64(total)
	if pct > 100 {
		return 100
	}
	return pct
}

// SendChat 发送文字消息（chat.send，chat.message 由 WS 回落）。
// type 字段是服务端 chat.send 的必填校验（v1.1.0 §0.12：本期仅 text）。
//
// nonce（#31）：每次用户发送生成一次；断线/写失败时消息连同 nonce 入队，
// 重连后由 OnConnect 用同一个 nonce 重发，服务端据此去重（只落一条、只广播一次）。
func (s *Session) SendChat(channelID, content string) error {
	return s.SendChatWithMeta(channelID, content, "", nil)
}

// SendChatWithMeta 发送带回复与 @提及的消息（#39）。replyTo 空串 = 普通消息；
// mentions 为空 = 无提及。两者与 nonce 一起入离线队列，重连重放不丢字段。
func (s *Session) SendChatWithMeta(channelID, content, replyTo string, mentions []string) error {
	return s.SendChatWithAttachments(channelID, content, replyTo, mentions, nil)
}

// SendChatWithAttachments 发送带附件 ID 的消息（#41）。content 可为空串
// （纯附件消息）；attachmentIDs 与回复/提及一样随 nonce 入离线队列，重连
// 重发原样带回——否则弱网下附件消息会掉附件。
func (s *Session) SendChatWithAttachments(channelID, content, replyTo string, mentions, attachmentIDs []string) error {
	if s.ws == nil || s.ws.Closed() {
		return fmt.Errorf("session not connected")
	}
	nonce, err := newChatNonce()
	if err != nil {
		return err
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if len(s.pendingChats) >= maxPendingChats {
		return fmt.Errorf("离线消息队列已满（%d 条），请等连接恢复后重试", maxPendingChats)
	}
	// 复制 mentions/attachmentIDs：调用方可能复用切片，入队后内容必须以调用时的快照为准。
	var ms []string
	if len(mentions) > 0 {
		ms = append([]string(nil), mentions...)
	}
	var as []string
	if len(attachmentIDs) > 0 {
		as = append([]string(nil), attachmentIDs...)
	}
	s.pendingChats = append(s.pendingChats, pendingChat{
		channelID: channelID, content: content, nonce: nonce,
		replyTo: replyTo, mentions: ms, attachmentIDs: as,
	})
	s.flushPendingChatsLocked()
	return nil
}

// maxPendingChats 断线期间积压的聊天消息上限：超出直接拒绝，不无限占内存。
const maxPendingChats = 64

// pendingChat 一条待发消息。nonce 在用户发送时生成一次，重试不换。
type pendingChat struct {
	channelID     string
	content       string
	nonce         string
	replyTo       string   // #39 回复目标消息 ID（空 = 非回复）
	mentions      []string // #39 被 @ 的用户 ID
	attachmentIDs []string // #41 随消息发送的附件 ID（重连重发必须保留）
}

// flushPendingChats 重连后重发积压消息（OnConnect 在 ws 读循环上调用）。
func (s *Session) flushPendingChats() {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	s.flushPendingChatsLocked()
}

// flushPendingChatsLocked 按入队顺序发送；首个失败即停，保留剩余项等下次重连。
// 调用方持 chatMu（与 SendChat 串行，保证重发与新消息的相对顺序）。
func (s *Session) flushPendingChatsLocked() {
	for len(s.pendingChats) > 0 {
		if err := s.sendChatFrame(s.pendingChats[0]); err != nil {
			return
		}
		s.pendingChats = s.pendingChats[1:]
	}
}

// sendChatFrame 发送一条 chat.send。nonce 的 x-since 是 2：老服务端不协商协议
// （protoVer=0），不带该字段，wire 与 v1 完全一致；v2 服务端才携带。
func (s *Session) sendChatFrame(item pendingChat) error {
	d := signal.ChatSendData{
		ChannelID: item.channelID, Type: "text", Content: item.content,
		ReplyTo: item.replyTo, Mentions: item.mentions,
		AttachmentIDS: item.attachmentIDs,
	}
	if s.ProtocolVersion() >= 2 {
		d.Nonce = item.nonce
	}
	return s.ws.Send(signal.TypeChatSend, d)
}

// newChatNonce 16 字节 crypto/rand 的 hex（32 字符，满足 REST/WS 的 ≤64 上限）。
func newChatNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成消息 nonce 失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// RequestVoiceState 请求目标成员闭麦/静音（v0.1.2 voice.request_state；
// 纯请求语义，目标接受后自行执行，服务端无强制）。
func (s *Session) RequestVoiceState(channelID, targetID string, mute, deafen bool) error {
	if s.ws == nil {
		return fmt.Errorf("session not connected")
	}
	return s.ws.Send(signal.TypeVoiceRequestState, signal.VoiceRequestStateData{
		ChannelID: channelID,
		TargetID:  targetID,
		Mute:      &mute,
		Deafen:    &deafen,
	})
}

// VoiceStateView 供 UI 销毁重载后回填的语音态快照（v0.1.2）。
type VoiceStateView struct {
	ChannelID string     `json:"channel_id"`
	Muted     bool       `json:"muted"`
	Deafened  bool       `json:"deafened"`
	Members   []PeerView `json:"members"`
}

// VoiceState 当前语音态快照（channelId 空 = 未在频道）。
func (s *Session) VoiceState() VoiceStateView {
	v := VoiceStateView{
		ChannelID: s.roomID.Load().(string),
		Muted:     s.muted.Load(),
		Deafened:  s.deafened.Load(),
		Members:   s.Members(),
	}
	return v
}

// ChatHandler 注入 chat.message 处理（app 层补挂）。
// msgID 为服务端消息 ID（文字消息按频道隔离与去重需要真实 ID，
// 此前回调丢弃了 ID，前端只能拿时间戳当 key，无法可靠去重）。
// nonce 为发送方 chat.send 携带的幂等键（#31，协议 v2；REST 补拉与老服务端为空串）。
// 同时记录收过消息的频道（#32 全量补拉的范围），fn 回灌路径复用同一回调。
// 旧签名不含 #39 的 reply/mentions 字段；新调用方用 ChatMessageHandler，
// 两者同时设置时以 ChatMessageHandler 为准（只投递一次）。
func (s *Session) ChatHandler(fn func(channelID, msgID, userID, nickname, content, ts, nonce string)) {
	if s.ws == nil {
		return
	}
	s.mu.Lock()
	s.chatFn = fn
	s.mu.Unlock()
}

// ChatMessageHandler 注入富字段 chat.message 处理（#39）：除旧字段外还带
// reply_to / reply_preview / mentions。与 ChatHandler 二选一；补拉
// （fullResync）回灌同样走这里，字段口径与实时广播一致。
// 回调跑在 ws 读循环 goroutine，实现侧只做转发，不得阻塞、不得同步 Close 会话。
func (s *Session) ChatMessageHandler(fn func(d signal.ChatMessageData)) {
	if s.ws == nil {
		return
	}
	s.mu.Lock()
	s.chatMsgFn = fn
	s.mu.Unlock()
}

// handleChatMessage chat.message 的统一分发：记录频道补拉范围后交给注入的回调。
// 注册在 registerHandlers 里，只注册一次。
func (s *Session) handleChatMessage(env signal.Envelope) {
	var d signal.ChatMessageData
	if env.Decode(&d) != nil {
		return
	}
	s.mu.Lock()
	s.seenChans[d.ChannelID] = true
	rich := s.chatMsgFn
	legacy := s.chatFn
	s.mu.Unlock()
	if rich != nil {
		rich(d)
		return
	}
	if legacy != nil {
		legacy(d.ChannelID, d.ID, d.User.ID, d.User.Nickname, d.Content,
			fmt.Sprintf("%d", d.TS), d.Nonce)
	}
}

// chatMessageDataFromDTO REST 消息 → chat.message 载荷（#32/#78 补拉回灌用）：
// 与实时广播同口径，保留回复目标、预览、提及与附件。
func chatMessageDataFromDTO(m api.MessageDTO) signal.ChatMessageData {
	d := signal.ChatMessageData{
		ID:        m.ID.String(),
		ChannelID: m.ChannelID.String(),
		Content:   m.Content,
		TS:        m.CreatedAt.UnixMilli(),
		User:      signal.UserBrief{ID: m.UserID.String(), Nickname: m.SenderNickname},
	}
	if m.ReplyTo != nil {
		d.ReplyTo = m.ReplyTo.String()
	}
	if p := m.ReplyPreview; p != nil {
		d.ReplyPreview = &signal.ReplyPreview{
			ID: p.ID.String(), UserID: p.UserID.String(),
			Nickname: p.Nickname, Content: p.Content, Deleted: p.Deleted,
		}
	}
	for _, id := range m.Mentions {
		d.Mentions = append(d.Mentions, id.String())
	}
	// #41：软删消息不下发附件；附件保持服务端相对 url，由 app 层拼基址。
	if m.DeletedAt == nil && m.Attachments != nil {
		for _, a := range *m.Attachments {
			d.Attachments = append(d.Attachments, signal.AttachmentInfo{
				ID: a.ID.String(), Filename: a.Filename, Size: a.Size,
				ContentType: a.ContentType, Width: int64(a.Width), Height: int64(a.Height),
				URL: a.URL, ThumbnailURL: a.ThumbnailURL,
			})
		}
	}
	return d
}

// resyncRecentMessages 单频道补拉的消息条数（最新 N 条，覆盖断线窗口）。
const resyncRecentMessages = 50

// applyFriendPresence 好友在线态回灌（状态型更新，幂等）；频道快照字段（#92）
// 与在线态一起透传，UI 刷新好友所在语音房。
func (s *Session) applyFriendPresence(friends []api.FriendDTO) {
	if s.ev.OnPresenceUpdate == nil {
		return
	}
	for _, f := range friends {
		if f.Status == "accepted" {
			s.ev.OnPresenceUpdate(f.User.ID.String(), f.User.Online, f.ChannelID, f.ServerID)
		}
	}
}

// fullResync 事件面续传失败后的全量补拉（#32/#78）。跑在独立 goroutine
// （gateway.Tracker 异步触发），可安全做 REST I/O——WS 读循环禁止阻塞。
// 范围：
//  1. 聚合快照 GET /api/v1/sync 一次取回服务器/频道/好友；好友在线态回灌
//     OnPresenceUpdate。快照本身不落会话状态（服务器/频道视图由 UI 收到
//     Events.OnResync 后经绑定自取）；失败只降级好友单拉，不影响后续消息补拉。
//  2. 本会话收过消息的频道各拉最近消息，回灌 ChatHandler（前端按消息 id 幂等，
//     与在途增量重叠也不重复）。聚合端点不含消息——按用户全部频道返消息的
//     响应体积不可控，消息仍按 seenChans 范围补拉。
//  3. Events.OnResync 通知 UI 层刷新服务器/频道等 REST 视图（只发提示不代拉）。
func (s *Session) fullResync() {
	s.mu.Lock()
	chans := make([]string, 0, len(s.seenChans))
	for ch := range s.seenChans {
		chans = append(chans, ch)
	}
	chatFn := s.chatFn
	chatMsgFn := s.chatMsgFn
	s.mu.Unlock()

	if snap, err := s.api.Sync(); err != nil {
		s.log.Printf("gateway: 聚合快照拉取失败，好友回落到单拉: %v", err)
		if friends, ferr := s.api.ListFriends(); ferr != nil {
			s.log.Printf("gateway: 补拉好友在线态失败: %v", ferr)
		} else {
			s.applyFriendPresence(friends)
		}
	} else {
		s.applyFriendPresence(snap.Friends)
	}

	for _, ch := range chans {
		cid, err := strconv.ParseInt(ch, 10, 64)
		if err != nil {
			continue // dev 内置房间无 REST 实体，无历史可拉
		}
		msgs, err := s.api.ListMessages(cid, resyncRecentMessages)
		if err != nil {
			s.log.Printf("gateway: 补拉频道 %s 消息失败: %v", ch, err)
			continue
		}
		if chatMsgFn == nil && chatFn == nil {
			continue
		}
		for _, m := range msgs {
			if chatMsgFn != nil {
				chatMsgFn(chatMessageDataFromDTO(m))
				continue
			}
			chatFn(ch, m.ID.String(), m.UserID.String(), m.SenderNickname, m.Content,
				strconv.FormatInt(m.CreatedAt.UnixMilli(), 10), "")
		}
	}

	if s.ev.OnResync != nil {
		s.ev.OnResync()
	}
}

// handleErrorFrame 服务端 error 帧的统一入口。先尝试按进房失败归因并回滚
// 房间态（#91），再转给 ErrorHandler/ErrorHandlerEx 注入的聊天错误回调。
//
// 归因约束：error 帧没有 request id（api/signal 契约），无法精确关联到上行
// 消息。这里只能按「pending join 队列非空 + 错误码」过滤，并按服务端在单
// 连接上的处理顺序把错误归属给最早未结算的 join。通用码（forbidden /
// bad_message / rate_limited / quota_exceeded）其他路径也会返回——尤其
// quota_exceeded 还来自 rtc.offer 的 SFU 容量上限（hub.handleRTCOffer）——
// 队列非空时存在误判的理论窗口；队列为空时一律按聊天错误处理。
func (s *Session) handleErrorFrame(env signal.Envelope) {
	var d signal.ErrorData
	if env.Decode(&d) != nil {
		return
	}
	joinOnly := isJoinOnlyErrorCode(d.Code)
	joinRelated := joinOnly || isJoinSharedErrorCode(d.Code)

	channelID := ""
	consumed, rolledBack := false, false
	if joinRelated {
		// already_in_channel 是「本连接已在该房」的确认，不是失败：只结算
		// pending，回滚会把重复点击时刚加入的有效房间清掉。
		channelID, consumed, rolledBack = s.settlePendingJoin(d.Code != signal.ErrCodeAlreadyInChan)
	}
	if !consumed && joinOnly {
		// 进房专用码（not_found/already_in_channel）不可能来自聊天。没有
		// pending 时是迟到/重复帧，静默丢弃，避免弹出「消息发送失败」。
		// quota_exceeded 不在此列：rtc.offer 配额失败同样用它，必须继续转发。
		return
	}
	if rolledBack && s.ev.OnJoinFailed != nil {
		s.ev.OnJoinFailed(channelID, d.Code, d.Message)
	}
	// consumed 的错误已归属进房路径（可能已回滚，也可能是无需提示的结算）：
	// 仍通知回调，但带 joinHandled=true 让 app 层跳过 chat:error，避免同一次
	// 失败弹两条提示；CLI 等老调用方经 ErrorHandler 仍能看到全部错误。
	s.mu.Lock()
	fns := append([]func(code, message string, joinHandled bool){}, s.errFns...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(d.Code, d.Message, consumed)
	}
}

// isJoinOnlyErrorCode 只会由 channel.join 返回的错误码（server/internal/signal/
// hub.go 的 authorizeChannelJoin / joinRoom 路径）。chat.send 不会返回这两个码，
// 无 pending 时可按迟到/重复帧静默丢弃。
func isJoinOnlyErrorCode(code string) bool {
	switch code {
	case signal.ErrCodeNotFound, signal.ErrCodeAlreadyInChan:
		return true
	}
	return false
}

// isJoinSharedErrorCode channel.join 与其他上行/协商都可能返回的通用码：
//   - forbidden / bad_message / rate_limited：聊天等上行共用；
//   - quota_exceeded：进房满员（hub.joinRoom）之外，rtc.offer 的 SFU 容量
//     上限（hub.handleRTCOffer，§0.1.3）与握手连接数上限也会返回——无 pending
//     时必须继续走聊天错误回调，不能当 join-only 丢弃。
//   - bad_password（#37）：频道密码错误，join 专属但走同一错误帧通道；
//     归入共享码让 pending 非空时按进房失败回滚并触发 OnJoinFailed。
func isJoinSharedErrorCode(code string) bool {
	switch code {
	case signal.ErrCodeForbidden, signal.ErrCodeBadMessage, signal.ErrCodeRateLimited,
		signal.ErrCodeQuotaExceeded, signal.ErrCodeBadPassword:
		return true
	}
	return false
}

// ErrorHandler 注入服务端 error 帧处理（app 层补挂）。
// 服务端对 chat.send 等消息的校验/拒收是异步 error 帧回落，
// 此前未订阅导致服务端拒收时消息凭空消失且无提示（阻断 B1）。
// 进房失败已由会话内部回滚并经 OnJoinFailed 上报，这里仍会收到（无法区分
// 入参），需要区分请用 ErrorHandlerEx。
func (s *Session) ErrorHandler(fn func(code, message string)) {
	if fn == nil {
		return
	}
	s.ErrorHandlerEx(func(code, message string, _ bool) { fn(code, message) })
}

// ErrorHandlerEx 同 ErrorHandler，额外告知该错误帧是否已被进房路径处理
// （joinHandled=true：可能是进房失败并已触发 OnJoinFailed，也可能是
// already_in_channel 这类无需提示的结算）。app 层据此跳过 chat:error，
// 避免同一次错误重复提示。
func (s *Session) ErrorHandlerEx(fn func(code, message string, joinHandled bool)) {
	if s.ws == nil || fn == nil {
		return
	}
	s.mu.Lock()
	s.errFns = append(s.errFns, fn)
	s.mu.Unlock()
}

// Close 释放全部资源。
func (s *Session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.ws != nil {
		s.ws.Close()
	}
	s.wg.Wait()
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	if s.remoteMgr != nil {
		s.remoteMgr.RemoveAll()
	}
	s.stopMedia()
	if s.mixer != nil {
		s.mixer.Stop()
	}
	if s.pipe != nil {
		_ = s.pipe.Close()
	}
	s.clearRoomTables()
}

// SignalConnected 报告信令 WS 当前是否有活跃连接（app 层据首次连接结果定 session_ok）。
func (s *Session) SignalConnected() bool { return s.ws != nil && s.ws.Connected() }

// ProtocolVersion 返回服务端协商到的协议版本（#29）。
// 0 = 尚未收到 server.hello（老服务端或还没协商完）——按 v1 行为处理。
func (s *Session) ProtocolVersion() int64 { return s.protoVer.Load() }

// SupportsWhisper 报告服务端是否支持同频道悄悄话（voice.whisper，协议 ≥3）。
// false 时 App 层不得开旁路发送门，UI 应明示不支持。
func (s *Session) SupportsWhisper() bool { return s.protoVer.Load() >= 3 }

// SupportsWhisperAck 报告服务端是否会回 voice.whisper_ack（协议 ≥4）。
// true 时客户端必须等到匹配 seq 的确认后才开麦（#116）。
func (s *Session) SupportsWhisperAck() bool { return s.protoVer.Load() >= 4 }

// InRoom 报告当前是否在语音频道（app 层 voice:stats ticker 据它门控发送）。
func (s *Session) InRoom() bool {
	room, _ := s.roomID.Load().(string)
	return room != ""
}

// IsLoopbackURL 判断服务器基址是否指向环回（127.0.0.0/8、::1、localhost）。
// 环回目标仅本机/内网 SFU 需要收集环回 ICE 候选；公网部署必须关闭，
// 否则本机环回地址会作为候选暴露给远端对端。
// 解析失败视为非环回（安全侧，不收集环回候选）。
func IsLoopbackURL(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// prefetchICE 预取 STUN/TURN 凭证并写入缓存。只在可阻塞的 goroutine 调用
// （JoinChannel 的 Wails 绑定线程）；WS 读循环上的 setupPeer 只读缓存（#13）。
// 失败留空缓存，由 cachedICE 降级空 ICE 列表——本机/内网直连仍可通。
func (s *Session) prefetchICE() {
	servers := s.fetchICEServers()
	s.iceMu.Lock()
	s.iceServers = servers
	s.iceMu.Unlock()
}

// cachedICE 读预取结果。零 I/O，专供 WS 读循环使用；空即降级空 ICE 列表。
func (s *Session) cachedICE() []webrtc.ICEServer {
	s.iceMu.RLock()
	defer s.iceMu.RUnlock()
	return s.iceServers
}

// fetchICEServers 拉取 STUN/TURN 凭证并转成 pion 格式。失败或服务端未配置
// TURN（dev 环境常见）时降级为空 ICE 列表——本机/内网直连仍可通，不阻断进房。
func (s *Session) fetchICEServers() []webrtc.ICEServer {
	resp, err := s.api.RTCCredentials()
	if err != nil {
		s.log.Printf("rtc credentials: %v（降级为空 ICE 列表）", err)
		return nil
	}
	if resp == nil || len(resp.ICEServers) == 0 {
		return nil
	}
	out := make([]webrtc.ICEServer, 0, len(resp.ICEServers))
	for _, srv := range resp.ICEServers {
		out = append(out, webrtc.ICEServer{
			URLs:       srv.URLs,
			Username:   srv.Username,
			Credential: srv.Credential,
		})
	}
	return out
}

// iceFromPion / iceToPion 信令线格式 ↔ pion（与 calltest 同款）。
func iceFromPion(c webrtc.ICECandidateInit) signal.ICEData {
	d := signal.ICEData{Candidate: c.Candidate, SDPMLineIndex: c.SDPMLineIndex}
	if c.SDPMid != nil {
		d.SDPMid = *c.SDPMid
	}
	if c.UsernameFragment != nil {
		d.UsernameFragment = *c.UsernameFragment
	}
	return d
}

func iceToPion(d signal.ICEData) webrtc.ICECandidateInit {
	c := webrtc.ICECandidateInit{Candidate: d.Candidate, SDPMLineIndex: d.SDPMLineIndex}
	if d.SDPMid != "" {
		c.SDPMid = &d.SDPMid
	}
	if d.UsernameFragment != "" {
		c.UsernameFragment = &d.UsernameFragment
	}
	return c
}
