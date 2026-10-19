package signal

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// 心跳与关闭参数（§0.7；测试可直接缩短 Hub 字段）。
const (
	defaultHeartbeatEvery = 25 * time.Second // protocol Ping 间隔
	defaultPongWait       = 10 * time.Second // 无 Pong 判定超时 → CloseNow
	defaultReadIdle       = 90 * time.Second // 读侧兜底：无数据读且无 pong 的最长寿
	kickSendWindow        = 400 * time.Millisecond
	// session.kicked 发送窗口（§0.6：300~500ms）
	writeTimeout = 10 * time.Second // 单条下行写超时
)

// Hub WS 信令 hub：连接管理、房间登记与广播（docs/落地方案.md §5.3）。
// 与 SFU 的桥接：channel.join 在 WS 层登记成员；PeerConnection 建立由 rtc.* 消息驱动。
// 多设备会话（v1.1.0）：conns 按 uid+dev 单连接索引，同设备顶替/踢线与 SFU 归属核对见 §0。
type Hub struct {
	sfu       *sfu.SFU
	auth      Authenticator
	sess      *auth.SessionIndex // 多设备会话索引（nil=跳过握手 sid 比对，测试/过渡）
	log       *log.Logger
	maxPer    int // 房间人数上限（与 SFU 一致）
	chatStore ChatStore
	limits    RateLimiter
	dev       bool          // 开发模式：放行非数字的内置房间（生产必须为 false）
	presence  PresenceStore // 在线状态（presence:{uid}，#11）；nil=不做 presence

	// 心跳参数（默认值见包常量；测试可缩短）
	heartbeatEvery time.Duration
	pongWait       time.Duration
	readIdle       time.Duration
	presenceTTL    time.Duration // 在线态 TTL（默认 defaultPresenceTTL，测试可缩短）

	mu         sync.Mutex
	rooms      map[string]*roomState
	conns      map[connKey]*client // uid+"\x00"+dev → 当前连接（§0.8）
	nextConnID uint64              // 连接标识自增（SFU peer 归属核对用）

	// evlog 事件面每会话事件环（#32）：seq 分配与断线续传补发。
	// 实现为进程内环形队列（单实例假设，见 gateway.go 头注释）。
	evlog EventLog

	// 配额与频控（#20）：maxConns 单 uid 连接数；joinRate/offerRate 每分钟每连接。
	maxConns  int
	joinRate  int
	offerRate int

	// protoMax 服务端宣布的协议版本上限（LITETALK_PROTOCOL_MAX 逃生阀，#29）。
	// 0 = 用构建期 ProtocolVersionMax；启动前用 SetProtoMax 设置，运行期只读。
	protoMax int
}

// connKey 连接索引键：uid + "\x00" + dev（§0.8；NUL 分隔避免与 ID 内容歧义）。
type connKey string

func makeConnKey(uid, dev string) connKey { return connKey(uid + "\x00" + dev) }

// roomState 信令层房间：成员表与语音状态（全内存态，M3 换成频道模型）。
// members 按 uid 索引且同 uid 仅一个连接（多设备会话切换由 join 抢占完成）。
type roomState struct {
	id      string
	members map[string]*client // userID → client
}

// client 一条 WS 连接。
type client struct {
	hub      *Hub
	conn     *websocket.Conn
	userID   string
	username string
	tag      string // 昵称#tag 的 tag（join 时从 store 缓存，成员快照填充）
	sid      string // 会话 ID（握手比对后的声明值；dev 令牌为空）
	dev      string // 设备类型（desktop/…）
	connID   uint64 // Hub 自增连接标识；SFU peer 挂此值做归属核对（§0.9）

	send       chan []byte // 下行队列（单写者 goroutine 消费）
	readCancel context.CancelFunc
	lastAct    atomic.Int64 // 最近活动（UnixNano）：数据读或心跳 pong
	closing    atomic.Bool  // CloseNow 幂等

	mu     sync.Mutex
	roomID string // 当前所在频道（空 = 未加入）
	state  VoiceStateData
	rate   map[string]*rateWin // 单连接消息频控窗口（#20，随连接销毁丢弃）

	// protoVer 协议版本（#29）：protoVerUnnegotiated=未收 client.hello（按 v1 处理）、
	// protoVerIncompatible=版本无交集（拒收业务消息但不断连，裁定 #9）、≥1=协商结果。
	// 读循环 goroutine 写、任意 goroutine 读 → atomic。
	protoVer atomic.Int64
}

// rateWin 固定窗计数（1 分钟窗，#20）。
type rateWin struct {
	start time.Time
	n     int
}

// closeNow 立即断连（心跳超时/写失败/队列满）：幂等；取消读循环 ctx 并跳过关闭握手。
func (c *client) closeNow() {
	if c.closing.Swap(true) {
		return
	}
	if c.readCancel != nil {
		c.readCancel()
	}
	_ = c.conn.CloseNow()
}

// ChatDeps 聊天持久化与频控依赖（可零值：仅广播不落库、不限流）。
type ChatDeps struct {
	Store  ChatStore
	Limits RateLimiter
	// Presence 在线状态存储（presence:{uid}）与好友关系查询（#11）；nil 不做 presence。
	Presence PresenceStore
	// Dev 开发模式：放行非数字的内置房间 ID（M2 遗留场景，无 store 频道实体）。
	// 生产构建（注入了版本号）必须为 false，否则任意字符串房间都可进入。
	Dev bool
	// MaxConns 单 uid 的 WS 连接数上限（多设备，#20）；0 = 不限。
	MaxConns int
	// JoinRate / OfferRate 单连接 channel.join、rtc.offer 的每分钟次数上限（#20）；0 = 不限。
	JoinRate  int
	OfferRate int
	// Backlog 每会话事件环条数（LITETALK_GATEWAY_BACKLOG，#32）；0 = 默认 256。
	Backlog int
}

// NewHub 构造信令 hub。maxMembers <= 0 时取 SFU 的上限。
// sess 为多设备会话索引：非 nil 时 WS 握手比对 claims.Sid（§0.4）；nil 跳过（测试/过渡）。
func NewHub(s *sfu.SFU, authn Authenticator, sess *auth.SessionIndex, logger *log.Logger, maxMembers int, chat ChatDeps) *Hub {
	if maxMembers <= 0 {
		maxMembers = s.MaxMembers()
	}
	if logger == nil {
		logger = log.Default()
	}
	h := &Hub{
		sfu:            s,
		auth:           authn,
		sess:           sess,
		log:            logger,
		maxPer:         maxMembers,
		chatStore:      chat.Store,
		limits:         chat.Limits,
		presence:       chat.Presence,
		dev:            chat.Dev,
		maxConns:       chat.MaxConns,
		joinRate:       chat.JoinRate,
		offerRate:      chat.OfferRate,
		heartbeatEvery: defaultHeartbeatEvery,
		pongWait:       defaultPongWait,
		readIdle:       defaultReadIdle,
		rooms:          make(map[string]*roomState),
		conns:          make(map[connKey]*client),
		evlog:          newMemEventLog(chat.Backlog),
	}
	// SFU 说话翻转 → 网关事件面（#33）：判定在 SFU ticker，下发在 WS 层。
	s.SetOnSpeaking(h.broadcastVoiceSpeaking)
	return h
}

// SetProtoMax 设置服务端宣布的协议版本上限（LITETALK_PROTOCOL_MAX 逃生阀）。
// n <= 0 恢复默认（构建期 ProtocolVersionMax）。仅限启动阶段调用。
func (h *Hub) SetProtoMax(n int) { h.protoMax = n }

// ServeWS 处理 GET /ws?token=<jwt|dev-xxx>。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	id, err := h.auth(r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// 握手比对会话 sid（§0.4）：不符 → 401 + X-LiteTalk-Auth: session_replaced（body code 同值）。
	if !h.sessionOK(r.Context(), id) {
		w.Header().Set("X-LiteTalk-Auth", "session_replaced")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "session_replaced",
			"message": "账号已在其他设备登录",
		})
		return
	}
	// 连接数配额（#20）：单 uid 超上限拒握手（同 uid+dev 的重连是顶替，不占新名额）。
	// 必须在 Accept 之前拒绝——升级后再写状态码已无效。
	if !h.connQuotaOK(id.UserID, id.Dev) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": ErrCodeQuotaExceeded, "message": "连接数已达上限",
		})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// M2 开发阶段放开 Origin 校验；M3 上线前必须配置 OriginPatterns
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Printf("ws: 升级失败: %v", err)
		return
	}
	conn.SetReadLimit(256 * 1024) // SDP 较大，放宽到 256KB

	ctx, cancel := context.WithCancel(r.Context())
	c := &client{
		hub:        h,
		conn:       conn,
		userID:     id.UserID,
		username:   id.Username,
		sid:        id.Sid,
		dev:        id.Dev,
		send:       make(chan []byte, 256),
		readCancel: cancel,
	}
	c.lastAct.Store(time.Now().UnixNano())

	// 注册（§0.6 踢人序）：先登记新连接，再处理被顶替的旧连接。
	old := h.register(c)
	// 事件环登记（#32）：会话（sid）订阅随首次连接建立，断线后环保留供 resume 补发。
	h.evlog.Bind(c.sid, c.userID)
	go c.writeLoop(ctx)
	go h.heartbeatLoop(ctx, c)
	if old != nil {
		h.replace(old, c)
	}
	h.markOnline(c)

	h.sendJSON(c, TypeHello, HelloData{
		User:     UserBrief{ID: id.UserID, Nickname: id.Username},
		Servers:  []any{},
		Friends:  []any{},
		Presence: map[string]any{},
	})
	h.log.Printf("ws: %s 已连接（dev=%s conn=%d）", id.UserID, id.Dev, c.connID)

	defer h.cleanup(c)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return // 断连：cleanup 统一收尾
		}
		c.lastAct.Store(time.Now().UnixNano())
		h.handleMessage(c, data)
	}
}

// legacyTokenGrace 旧版本令牌（无 sid）的过渡期：服务端升级后已签发的 access
// 在有效期内仍可握手/REST，给用户留出重新登录窗口（P1-3）。令牌本身 2h TTL，
// 故取 2h+5min 缓冲；超期一律按 session_replaced 拒。
const legacyTokenGrace = 2*time.Hour + 5*time.Minute

// sessionOK 握手会话校验（§0.4）：claims.Sid 须与 SessionIndex 当前 sid 一致。
// 旧版本令牌（sid 空）在 legacyTokenGrace 过渡期内放行；sess 为 nil（测试/过渡）、
// devAuth 的 dev-令牌按各自规则处理。
func (h *Hub) sessionOK(ctx context.Context, id Identity) bool {
	if h.sess == nil || id.SkipSessionCheck {
		return true
	}
	uid, err := strconv.ParseInt(id.UserID, 10, 64)
	if err != nil {
		return false // 非数字 uid（dev 令牌）无会话可比对
	}
	if id.Sid == "" {
		// 旧版本令牌：过渡期内放行（兼容 v1.0.x 客户端），超期拒绝。
		return id.IssuedAt > 0 && time.Since(time.Unix(id.IssuedAt, 0)) < legacyTokenGrace
	}
	cur, err := h.sess.Get(ctx, uid, id.Dev)
	if err != nil || cur == "" || id.Sid != cur {
		return false
	}
	return true
}

// register 登记连接并分配 connID；返回同 uid+dev 的旧连接（nil=无）。
func (h *Hub) register(c *client) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextConnID++
	c.connID = h.nextConnID
	key := makeConnKey(c.userID, c.dev)
	old := h.conns[key]
	h.conns[key] = c
	return old
}

// unregister 仅当 conns[key] 仍是本连接时摘除（被顶替的旧连接不得误删新连接，§0.8）。
func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	key := makeConnKey(c.userID, c.dev)
	if h.conns[key] == c {
		delete(h.conns, key)
	}
	h.mu.Unlock()
}

// replace 处理同 uid+dev 的旧连接（§0.6）：同 sid = 会话半开被替换 → 静默 close 4000；
// 否则 = 同类设备新登录 → 旧连接先收 session.kicked，发送窗口后 close 4001。异步执行，不阻塞新连接。
func (h *Hub) replace(old, c *client) {
	if old.sid != "" && old.sid == c.sid {
		go func() {
			_ = old.conn.Close(CloseCodeSessionReplaced, "session replaced")
		}()
		return
	}
	go func() {
		h.sendEvent(old, TypeSessionKicked, SessionKickedData{
			Reason:     KickReasonLoginElsewhere,
			DeviceType: old.dev,
		})
		time.Sleep(kickSendWindow) // 踢人发送窗口（§0.6：300~500ms）
		_ = old.conn.Close(CloseCodeNewLogin, "login elsewhere")
	}()
}

// Kick 踢掉该用户的 WS 连接：dev 为空 = 全部设备（登出/重置密码/语音抢占共用，§0.11）。
// 每个目标：尽力发 session.kicked（reason 空回落 login_elsewhere）→ 发送窗口 → close 4001。
func (h *Hub) Kick(uid, dev, reason string) {
	if reason == "" {
		reason = KickReasonLoginElsewhere
	}
	h.mu.Lock()
	var targets []*client
	for k, c := range h.conns {
		if !strings.HasPrefix(string(k), uid+"\x00") {
			continue
		}
		if dev != "" && string(k) != string(makeConnKey(uid, dev)) {
			continue
		}
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		go func(c *client) {
			h.sendEvent(c, TypeSessionKicked, SessionKickedData{Reason: reason, DeviceType: c.dev})
			time.Sleep(kickSendWindow)
			_ = c.conn.Close(CloseCodeNewLogin, "login elsewhere")
		}(c)
	}
}

// Shutdown 优雅关闭广播（#21）：对全部连接投递 server.shutdown，150ms 发送窗口后 CloseNow。
func (h *Hub) Shutdown(reason string) {
	if reason == "" {
		reason = "shutdown"
	}
	h.mu.Lock()
	targets := make([]*client, 0, len(h.conns))
	for _, c := range h.conns {
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		h.sendEvent(c, TypeServerShutdown, ServerShutdownData{Reason: reason, TS: time.Now().UnixMilli()})
	}
	if len(targets) > 0 {
		time.Sleep(150 * time.Millisecond) // 发送窗口：让下行队列把 shutdown 消息写出去
	}
	for _, c := range targets {
		c.closeNow()
	}
}

// KickChannel 把该频道房间内的全部连接强制离房（#22，频道/服务器删除后收尾）。
// 只清房间归属与 SFU peer，不动 WS 连接本身——被摘连接收 session.voice_taken
// 做本地退房（复用既有「服务端单方面摘出房间」路径）。
// 单实例语义：跨实例部署踢不到其他实例上的连接。
func (h *Hub) KickChannel(channelID, reason string) {
	h.mu.Lock()
	room := h.rooms[channelID]
	if room == nil {
		h.mu.Unlock()
		return
	}
	var pending []leaveWork
	var removed []*client
	for _, c := range room.members {
		if w := h.leaveWorkLocked(c); w != nil {
			pending = append(pending, *w)
			removed = append(removed, c)
		}
	}
	h.mu.Unlock()
	for _, w := range pending {
		h.finishLeave(w)
	}
	if reason != "" {
		h.log.Printf("ws: 频道 %s 被移除（%s），%d 人已离房", channelID, reason, len(removed))
	}
	for _, c := range removed {
		h.sendJSON(c, TypeSessionVoiceTaken, nil)
	}
}

// writeLoop 唯一写者：保证 coder/websocket 单写者约束。
// 出错（写超时/对端关闭）→ cancel 读循环 ctx + CloseNow（§0.7）。
func (c *client) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if !c.closing.Load() {
				_ = c.conn.Close(websocket.StatusNormalClosure, "server shutdown")
			}
			return
		case msg := <-c.send:
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				c.closeNow()
				return
			}
		}
	}
}

// heartbeatLoop 协议层心跳（§0.7）：每 heartbeatEvery 发 Ping，pongWait 内无 Pong → CloseNow；
// 读侧兜底：readIdle 内无任何活动（数据读或 pong）→ CloseNow（僵尸连接清理）。
// Ping 依赖读循环并发处理 pong（coder/websocket 在 Read 中自动回 Pong），与 writeLoop 并发安全。
func (h *Hub) heartbeatLoop(ctx context.Context, c *client) {
	every := h.heartbeatEvery
	if every <= 0 {
		every = defaultHeartbeatEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if last := c.lastAct.Load(); last > 0 && time.Since(time.Unix(0, last)) > h.readIdle {
				h.log.Printf("ws: %s 读侧 %v 无活动，关闭僵尸连接", c.userID, h.readIdle)
				c.closeNow()
				return
			}
			pctx, pcancel := context.WithTimeout(ctx, h.pongWait)
			err := c.conn.Ping(pctx)
			pcancel()
			if err != nil {
				h.log.Printf("ws: %s 心跳失败（%v 内无 Pong）: %v", c.userID, h.pongWait, err)
				c.closeNow()
				return
			}
			c.lastAct.Store(time.Now().UnixNano())
			h.renewPresence(c) // 心跳成功续期 presence:{uid}（25s 一拍，TTL 90s）
			// 心跳成功说明连接活跃：续期事件环。Append 不再顺带续期（否则广播
			// 会永久保住断线旧 sid 的环），没有这一拍，30 分钟没消息的在线会话
			// 会被 sweep 误淘汰、之后 resume 得到 unknown_session。
			h.evlog.Touch(c.sid)
		}
	}
}

// cleanup 断连收尾：摘连接索引、离房（触发 SFU 归属清理 + peer_left 广播）、
// 该 uid 无其它连接时下线 presence。均做身份核对（conns[key]==c、members[uid]==c），
// 被顶替的旧连接不会误删新连接（§0.8）。多处调用（defer/心跳超时/写失败），全程幂等。
func (h *Hub) cleanup(c *client) {
	h.unregister(c)
	h.leaveRoom(c)
	h.markOffline(c.userID)
	h.log.Printf("ws: %s 已断开", c.userID)
}

// sendJSON 编码并投递下行消息；队列满 = 对端消费停滞 → CloseNow（§0.7，不静默丢 rtc.offer/ice）。
func (h *Hub) sendJSON(c *client, msgType string, data any) {
	raw, err := MarshalEnvelope(msgType, data)
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

func (h *Hub) sendError(c *client, code, message string) {
	h.sendJSON(c, TypeError, ErrorData{Code: code, Message: message})
}

// protoVer 哨兵值（#29）。
const (
	protoVerUnnegotiated = 0  // 未收到 client.hello：按协议 v1 处理（老客户端路径）
	protoVerIncompatible = -1 // 版本无交集：拒收业务消息，但绝不主动断连（裁定 #9）
)

// handleMessage 分发一条上行信令。
func (h *Hub) handleMessage(c *client, data []byte) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		h.sendError(c, ErrCodeBadMessage, "信封不是合法 JSON")
		return
	}
	// 协商消息优先于版本门控：无交集后仍允许重复收到 client.hello（回同样的拒绝）。
	if env.Type == TypeClientHello {
		h.handleClientHello(c, env)
		return
	}
	if c.protoVer.Load() == protoVerIncompatible {
		h.sendError(c, ErrCodeProtocolUnsupported, "协议版本无交集，本连接不接受业务消息")
		return
	}
	switch env.Type {
	case TypeChannelJoin:
		var d ChannelJoinData
		if err := env.Decode(&d); err != nil || d.ChannelID == "" {
			h.sendError(c, ErrCodeBadMessage, "channel.join 需要 channel_id")
			return
		}
		// join 频控（#20）：单连接每分钟上限，超限 rate_limited。
		if !h.allowRate(c, TypeChannelJoin, h.joinRate) {
			h.sendError(c, ErrCodeRateLimited, "入房过于频繁")
			return
		}
		if code, msg := h.authorizeChannelJoin(c, d.ChannelID); code != "" {
			h.sendError(c, code, msg)
			return
		}
		h.joinRoom(c, d.ChannelID)
	case TypeChannelLeave:
		h.leaveRoom(c)
	case TypeRTCOffer:
		var d SDPData
		if err := env.Decode(&d); err != nil || d.SDP == "" {
			h.sendError(c, ErrCodeBadMessage, "rtc.offer 需要 sdp")
			return
		}
		// offer 频控（#20）：单连接每分钟上限，超限 rate_limited。
		if !h.allowRate(c, TypeRTCOffer, h.offerRate) {
			h.sendError(c, ErrCodeRateLimited, "协商过于频繁")
			return
		}
		h.handleRTCOffer(c, d.SDP)
	case TypeRTCAnswer:
		var d SDPData
		if err := env.Decode(&d); err != nil || d.SDP == "" {
			h.sendError(c, ErrCodeBadMessage, "rtc.answer 需要 sdp")
			return
		}
		h.handleRTCAnswer(c, d.SDP)
	case TypeRTCIce:
		var d ICEData
		if err := env.Decode(&d); err != nil {
			h.sendError(c, ErrCodeBadMessage, "rtc.ice 需要 candidate")
			return
		}
		h.handleRTCCandidate(c, toICECandidate(d))
	case TypeVoiceState:
		var d VoiceStateData
		if err := env.Decode(&d); err != nil {
			h.sendError(c, ErrCodeBadMessage, "voice.state 数据格式错误")
			return
		}
		h.handleVoiceState(c, d)
	case TypeVoiceRequestState:
		var d VoiceRequestStateData
		if env.Decode(&d) != nil {
			h.sendError(c, ErrCodeBadMessage, "voice.request_state 数据格式错误")
			return
		}
		h.handleVoiceRequestState(c, d)
	case TypeChatSend:
		var d ChatSendData
		if err := env.Decode(&d); err != nil {
			h.sendError(c, ErrCodeBadMessage, "chat.send 数据格式错误")
			return
		}
		h.handleChatSend(c, d)
	case TypeGatewayResume:
		var d GatewayResumeData
		if err := env.Decode(&d); err != nil {
			h.sendError(c, ErrCodeBadMessage, "gateway.resume 数据格式错误")
			return
		}
		h.handleGatewayResume(c, d)
	default:
		h.sendError(c, ErrCodeBadMessage, "未知消息类型: "+env.Type)
	}
}

// handleClientHello 协议版本协商（#29）。
//
//	选定 v = min(serverMax, clientMax)，且须 v ≥ max(clientMin, serverMin)。
//
// 无交集只回 error{protocol_unsupported} 并拒收后续业务消息，**绝不主动断连**（裁定 #9）——
// 断连会触发客户端重连风暴，且老服务端→新客户端的不兼容提示需要在带内送达。
// 老客户端不发本消息：protoVer 保持 protoVerUnnegotiated，按 v1 处理、行为与今天一致。
func (h *Hub) handleClientHello(c *client, env Envelope) {
	var d ClientHelloData
	if err := env.Decode(&d); err != nil {
		h.sendError(c, ErrCodeBadMessage, "client.hello 需要 min 与 max")
		return
	}
	if d.Min < 1 || d.Max < d.Min {
		h.sendError(c, ErrCodeBadMessage, "client.hello 版本区间非法")
		return
	}
	// 重复协商只回当前结果，不重开区间（防止先报宽区间、再收窄绕过选择）。
	if cur := c.protoVer.Load(); cur != protoVerUnnegotiated {
		if cur == protoVerIncompatible {
			h.sendError(c, ErrCodeProtocolUnsupported, "协议版本无交集")
			return
		}
		h.sendJSON(c, TypeServerHello, ServerHelloData{Version: cur, SessionID: c.sid})
		return
	}
	serverMax := h.serverMax()
	v, ok := negotiateProtocol(d.Min, d.Max, ProtocolVersionMin, serverMax)
	if !ok {
		c.protoVer.Store(protoVerIncompatible)
		h.sendError(c, ErrCodeProtocolUnsupported,
			"协议版本无交集：服务端 "+strconv.FormatInt(ProtocolVersionMin, 10)+".."+
				strconv.FormatInt(serverMax, 10)+"，客户端 "+strconv.FormatInt(d.Min, 10)+".."+
				strconv.FormatInt(d.Max, 10))
		return
	}
	c.protoVer.Store(v)
	h.sendJSON(c, TypeServerHello, ServerHelloData{Version: v, SessionID: c.sid})
	h.log.Printf("ws: %s 协商协议版本 v%d（客户端 %d..%d）", c.userID, v, d.Min, d.Max)
}

// negotiateProtocol 选定协议版本：v = min(serverMax, clientMax)，
// 且 v ≥ max(clientMin, serverMin)，否则无交集。区间非法（min<1 或 max<min）也视为无交集。
func negotiateProtocol(clientMin, clientMax, serverMin, serverMax int64) (int64, bool) {
	if clientMin < 1 || clientMax < clientMin {
		return 0, false
	}
	if serverMax < serverMin {
		serverMax = serverMin
	}
	v := min(clientMax, serverMax)
	if v < max(clientMin, serverMin) {
		return 0, false
	}
	return v, true
}

// serverMax 服务端宣布的协议版本上限：LITETALK_PROTOCOL_MAX 逃生阀与构建期
// ProtocolVersionMax 取小，且不低于 ProtocolVersionMin。
func (h *Hub) serverMax() int64 {
	m := int64(ProtocolVersionMax)
	if h.protoMax > 0 && int64(h.protoMax) < m {
		m = int64(h.protoMax)
	}
	if m < ProtocolVersionMin {
		m = ProtocolVersionMin
	}
	return m
}

// authorizeChannelJoin 进房权限校验（P0 S1）：返回 (错误码, 消息)，code 为空即放行。
// 数字频道要求存在、Type 为 voice、请求者是其所属服务器成员；
// 非数字房间是 M2 内置房间（无 store 频道实体），仅 dev 模式放行。
// 校验含 store 查询，调用方不得持有 hub.mu。
func (h *Hub) authorizeChannelJoin(c *client, channelID string) (code, msg string) {
	ctx := context.Background()
	chID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		if h.dev {
			return "", ""
		}
		return ErrCodeNotFound, "频道不存在"
	}
	if h.chatStore == nil {
		return ErrCodeNotFound, "频道不存在"
	}
	ch, err := h.chatStore.ChannelByID(ctx, chID)
	if err != nil {
		return ErrCodeNotFound, "频道不存在"
	}
	if ch.Type != store.ChannelVoice {
		return ErrCodeForbidden, "只能加入语音频道"
	}
	uid, err := strconv.ParseInt(c.userID, 10, 64)
	if err != nil {
		return ErrCodeForbidden, "无权进入该频道"
	}
	mem, err := h.chatStore.Member(ctx, ch.ServerID, uid)
	if err != nil {
		return ErrCodeForbidden, "无权进入该频道"
	}
	// 缓存 tag 到连接上（成员快照填充用；join 是低频路径，一次查询即可）
	c.mu.Lock()
	c.tag = mem.Tag
	c.mu.Unlock()
	return "", ""
}

// joinRoom 登记成员并向全房间重发 channel.joined（成员全量，§5.3 无 peer_joined 消息，
// 成员变化以全量下发，客户端做全量替换）。整个登记在一把 hub.mu 内原子完成。
// 语音单处（§0.10）：同 uid 其他会话在任一语音房 → 先发 session.voice_taken、
// 清其房间成员与 SFU peer，然后本连接进房。
func (h *Hub) joinRoom(c *client, channelID string) {
	// 兜底再校验一次：joinRoom 可能被 channel.join 之外的路径调用，权限不可绕。
	if code, msg := h.authorizeChannelJoin(c, channelID); code != "" {
		h.sendError(c, code, msg)
		return
	}
	h.mu.Lock()
	if c.roomIDLocked() == channelID {
		h.mu.Unlock()
		h.sendError(c, ErrCodeAlreadyInChan, "已在该频道中")
		return
	}
	var pending []leaveWork
	// 先离开旧房（成员摘除在锁内，SFU 清理与广播在锁外执行）。
	if c.roomIDLocked() != "" {
		if w := h.leaveWorkLocked(c); w != nil {
			pending = append(pending, *w)
		}
	}
	room := h.rooms[channelID]
	if room == nil {
		room = &roomState{id: channelID, members: make(map[string]*client)}
		h.rooms[channelID] = room
	}
	if len(room.members) >= h.maxPer {
		h.mu.Unlock()
		for _, w := range pending {
			h.finishLeave(w)
		}
		// 容量超限统一 quota_exceeded（§0.1.3）。
		h.sendError(c, ErrCodeQuotaExceeded, "语音房间已满")
		return
	}
	// 语音单处：摘同 uid 其他会话（锁内），通知与 SFU 清理走 pending（锁外）。
	var voiceTakens []*client
	for k, o := range h.conns {
		if o == c || !strings.HasPrefix(string(k), c.userID+"\x00") {
			continue
		}
		if o.roomIDLocked() == "" {
			continue
		}
		voiceTakens = append(voiceTakens, o)
		if w := h.leaveWorkLocked(o); w != nil {
			pending = append(pending, *w)
		}
	}
	// 抢占通知先于 channel.joined 下发；SFU 清理在锁外、joined 广播之前完成（§0.10）。
	for _, o := range voiceTakens {
		h.sendJSON(o, TypeSessionVoiceTaken, nil)
	}
	// 同房抢占会摘掉旧成员——若目标房因此被销毁则重建。
	room = h.rooms[channelID]
	if room == nil {
		room = &roomState{id: channelID, members: make(map[string]*client)}
		h.rooms[channelID] = room
	}
	room.members[c.userID] = c
	c.setRoomID(channelID)
	members := room.snapshotLocked()
	h.mu.Unlock()

	// 锁外：被抢占/自旧房的 SFU 归属清理 + peer_left 广播（peer_left 先于 joined，
	// 同房抢占时接收方按序应用后成员列表仍含本 uid）。
	for _, w := range pending {
		h.finishLeave(w)
	}

	h.log.Printf("ws: %s 加入频道 %s（%d 人）", c.userID, channelID, len(members))
	all := memberInfos(members)
	for _, m := range members {
		h.sendJSON(m, TypeChannelJoined, ChannelJoinedData{
			ChannelID: channelID,
			Members:   all,
			Self:      memberInfoOf(m),
		})
	}
}

// leaveWork 一次离房的锁内计算结果：锁外执行 SFU 清理与广播。
type leaveWork struct {
	c         *client
	roomID    string
	removed   bool // 本连接确实是该房成员（需要广播 peer_left）
	remaining []*client
}

// leaveWorkLocked 离房（调用方须持 h.mu）：清本连接 roomID、身份核对后摘成员。
// members[uid] 不是本连接（已被同账号新连接顶替）时不摘成员、不广播（§0.8 盲删防护）。
func (h *Hub) leaveWorkLocked(c *client) *leaveWork {
	roomID := c.roomIDLocked()
	if roomID == "" {
		return nil
	}
	c.setRoomID("")
	w := &leaveWork{c: c, roomID: roomID}
	room := h.rooms[roomID]
	if room == nil {
		return w
	}
	if room.members[c.userID] == c {
		delete(room.members, c.userID)
		w.removed = true
	}
	if len(room.members) == 0 {
		delete(h.rooms, roomID)
	} else {
		w.remaining = room.snapshotLocked()
	}
	return w
}

// finishLeave 离房收尾（锁外）：SFU 按 connID 归属清理 + peer_left 广播（仅真实摘除成员时）。
func (h *Hub) finishLeave(w leaveWork) {
	h.sfu.LeaveIf(w.roomID, w.c.userID, strconv.FormatUint(w.c.connID, 10))
	if !w.removed {
		return
	}
	h.log.Printf("ws: %s 离开频道 %s", w.c.userID, w.roomID)
	for _, m := range w.remaining {
		h.sendJSON(m, TypeChannelPeerLeft, PeerLeftData{
			ChannelID: w.roomID,
			UserID:    w.c.userID,
		})
	}
}

// leaveRoom 离房：移除成员、SFU 归属清理、向剩余成员广播 channel.peer_left。
func (h *Hub) leaveRoom(c *client) {
	h.mu.Lock()
	w := h.leaveWorkLocked(c)
	h.mu.Unlock()
	if w != nil {
		h.finishLeave(*w)
	}
}

// handleRTCOffer 初始 rtc.offer（C→S）→ SFU 应答 → rtc.answer（S→C）。
func (h *Hub) handleRTCOffer(c *client, sdp string) {
	roomID := c.roomIDLocked()
	if roomID == "" {
		h.sendError(c, ErrCodeNotInChannel, "请先 channel.join 再发起 rtc.offer")
		return
	}
	answer, err := h.sfu.HandleOffer(roomID, c.userID, strconv.FormatUint(c.connID, 10), sdp, &clientSignal{hub: h, c: c})
	if err != nil {
		code := ErrCodeRTC
		// 容量超限统一 quota_exceeded（§0.1.3）：每房人数 / 房间数。
		if errors.Is(err, sfu.ErrRoomFull) || errors.Is(err, sfu.ErrTooManyRooms) {
			code = ErrCodeQuotaExceeded
		}
		h.sendError(c, code, "RTC 协商失败: "+err.Error())
		return
	}
	h.sendJSON(c, TypeRTCAnswer, SDPData{SDP: answer})
}

func (h *Hub) handleRTCAnswer(c *client, sdp string) {
	roomID := c.roomIDLocked()
	if roomID == "" {
		h.sendError(c, ErrCodeNotInChannel, "不在频道中")
		return
	}
	if err := h.sfu.HandleAnswer(roomID, c.userID, sdp); err != nil {
		h.log.Printf("ws: %s 应用 rtc.answer 失败: %v", c.userID, err)
	}
}

func (h *Hub) handleRTCCandidate(c *client, cand ICECandidateData) {
	roomID := c.roomIDLocked()
	if roomID == "" {
		return
	}
	if err := h.sfu.AddICECandidate(roomID, c.userID, cand); err != nil {
		h.log.Printf("ws: %s 添加 ICE 候选失败: %v", c.userID, err)
	}
}

// handleVoiceState 更新内存态成员表并向房间其他成员广播（服务端不校验）。
// v2 客户端的 speaking 字段被忽略（#33 裁定 #5）：协议 2 起说话判定走 RFC 6464
// 电平（voice.speaking），voice.state 只承载 mute/deafen；v1 客户端照旧全收全发。
func (h *Hub) handleVoiceState(c *client, d VoiceStateData) {
	if c.protoVer.Load() >= 2 {
		d.Speaking = nil
	}
	h.mu.Lock()
	roomID := c.roomIDLocked()
	c.mu.Lock()
	if d.Speaking != nil {
		c.state.Speaking = d.Speaking
	}
	if d.Mute != nil {
		c.state.Mute = d.Mute
	}
	if d.Deafen != nil {
		c.state.Deafen = d.Deafen
	}
	c.mu.Unlock()
	var others []*client
	if room := h.rooms[roomID]; roomID != "" && room != nil {
		for uid, m := range room.members {
			if uid != c.userID {
				others = append(others, m)
			}
		}
	}
	h.mu.Unlock()

	if roomID == "" {
		h.sendError(c, ErrCodeNotInChannel, "不在频道中")
		return
	}
	// 下行广播的 user_id 由服务端补上；上行 VoiceStateData 不带（schema 同一载荷）。
	bcast := VoiceStateData{
		UserID:   c.userID,
		Speaking: d.Speaking,
		Mute:     d.Mute,
		Deafen:   d.Deafen,
	}
	for _, m := range others {
		h.sendJSON(m, TypeVoiceState, bcast)
	}
}

// handleVoiceRequestState 处理 C→S voice.request_state（v0.1.2）：
// 校验发送者与目标均在同一频道后，把请求**定向转发**给目标——
// 纯「请求」语义，不广播、不持久化、不强制（server-mute 属权限模型，超范围）。
// 频控 10 次/分/人：与 chat.send 同套路，防止对目标定向刷屏（review S6）。
const voiceRequestRateLimit, voiceRequestRateWindow = 10, time.Minute

func (h *Hub) handleVoiceRequestState(c *client, d VoiceRequestStateData) {
	if d.ChannelID == "" || d.TargetID == "" {
		h.sendError(c, ErrCodeBadMessage, "voice.request_state 需要 channel_id 与 target_id")
		return
	}
	if d.Mute == nil && d.Deafen == nil {
		h.sendError(c, ErrCodeBadMessage, "voice.request_state 至少携带 mute 或 deafen")
		return
	}
	if d.TargetID == c.userID {
		h.sendError(c, ErrCodeBadMessage, "不能对自己发起请求")
		return
	}
	if h.limits != nil {
		ok, err := h.limits.Allow(context.Background(), "vreq:"+c.userID, voiceRequestRateLimit, voiceRequestRateWindow)
		if err != nil || !ok {
			h.sendError(c, ErrCodeRateLimited, "请求过于频繁")
			return
		}
	}
	h.mu.Lock()
	room := h.rooms[d.ChannelID]
	var target *client
	if room != nil {
		// 发送者也必须在频道内（room.members[c.userID] 存在即隐含）。
		if _, in := room.members[c.userID]; in {
			target = room.members[d.TargetID]
		}
	}
	h.mu.Unlock()
	if target == nil {
		h.sendError(c, ErrCodeNotInChannel, "目标不在该频道中")
		return
	}
	h.sendJSON(target, TypeVoiceStateRequest, VoiceStateRequestData{
		From:   UserBrief{ID: c.userID, Nickname: c.username},
		Mute:   d.Mute,
		Deafen: d.Deafen,
		TS:     time.Now().UnixMilli(),
	})
}

// ---- Hub 带锁快照 ----

// 以下访问器测试与生产共用：hub.rooms / hub.conns / room.members 只允许在持有 h.mu 时
// 触碰，包外代码与测试一律改走这些方法。P0-3 的竞争正是测试在解锁后读 room.members，
// 与 cleanup → leaveWorkLocked 的持锁删除并发——读侧不收锁，写侧加锁也挡不住。
// 返回的 *client 由连接自身生命周期持有，超出锁后引用是安全的；其可变字段
// （roomID、state）仍须经 roomIDLocked / memberInfoOf 等访问器读。

// roomMember 房间内某 uid 的成员连接（房不存在或该 uid 不在房 → nil）。持 h.mu。
func (h *Hub) roomMember(channelID, uid string) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[channelID]
	if room == nil {
		return nil
	}
	return room.members[uid]
}

// roomMemberCount 房间成员数（房不存在 → 0）。持 h.mu。
func (h *Hub) roomMemberCount(channelID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[channelID]
	if room == nil {
		return 0
	}
	return len(room.members)
}

// connByDevice 某 uid+dev 的当前连接（不在线 → nil）。持 h.mu。
func (h *Hub) connByDevice(uid, dev string) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[makeConnKey(uid, dev)]
}

// connCount 某 uid 的全部设备连接数（O(连接数)，仅断言/排障用）。持 h.mu。
func (h *Hub) connCount(uid string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := uid + "\x00"
	n := 0
	for k := range h.conns {
		if strings.HasPrefix(string(k), prefix) {
			n++
		}
	}
	return n
}

// connQuotaOK 单 uid 连接数配额（#20）：同 uid+dev 的重连是顶替（净增 0），
// 不占新名额；maxConns<=0 不限。持 h.mu。
func (h *Hub) connQuotaOK(uid, dev string) bool {
	if h.maxConns <= 0 {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := makeConnKey(uid, dev)
	prefix := uid + "\x00"
	n := 0
	for k := range h.conns {
		if k != key && strings.HasPrefix(string(k), prefix) {
			n++
		}
	}
	return n < h.maxConns
}

// rateWindow 单连接消息频控窗长（固定窗 1 分钟，#20）。
const rateWindow = time.Minute

// allowRate 单连接消息频控（键 = 消息类型，计数在连接上、随 cleanup 销毁）。
// limit<=0 不限流；超限返回 false（调用方回 rate_limited）。
func (h *Hub) allowRate(c *client, msgType string, limit int) bool {
	if limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	w := c.rate[msgType]
	if w == nil || now.Sub(w.start) >= rateWindow {
		if c.rate == nil {
			c.rate = make(map[string]*rateWin, 2)
		}
		c.rate[msgType] = &rateWin{start: now, n: 1}
		return true
	}
	if w.n >= limit {
		return false
	}
	w.n++
	return true
}

// ---- client / roomState 辅助 ----

func (c *client) roomIDLocked() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roomID
}

func (c *client) setRoomID(id string) {
	c.mu.Lock()
	c.roomID = id
	c.mu.Unlock()
}

// snapshotLocked 调用方须持有 hub.mu。
func (r *roomState) snapshotLocked() []*client {
	out := make([]*client, 0, len(r.members))
	for _, m := range r.members {
		out = append(out, m)
	}
	return out
}

// memberInfoOf 单个客户端的成员快照（含锁）。
func memberInfoOf(m *client) MemberInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MemberInfo{
		UserID:   m.userID,
		Nickname: m.username,
		Tag:      m.tag,
		Mute:     m.state.Mute != nil && *m.state.Mute,
		Deafen:   m.state.Deafen != nil && *m.state.Deafen,
		Speaking: m.state.Speaking != nil && *m.state.Speaking,
	}
}

func memberInfos(members []*client) []MemberInfo {
	infos := make([]MemberInfo, 0, len(members))
	for _, m := range members {
		m.mu.Lock()
		infos = append(infos, MemberInfo{
			UserID:   m.userID,
			Nickname: m.username,
			Tag:      m.tag,
			Mute:     m.state.Mute != nil && *m.state.Mute,
			Deafen:   m.state.Deafen != nil && *m.state.Deafen,
			Speaking: m.state.Speaking != nil && *m.state.Speaking,
		})
		m.mu.Unlock()
	}
	return infos
}

// clientSignal 实现 sfu.SignalSender：SFU 主动下发的 offer / ICE 经 WS 送达客户端。
type clientSignal struct {
	hub *Hub
	c   *client
}

func (s *clientSignal) SendOffer(sdp string) {
	s.hub.sendJSON(s.c, TypeRTCOffer, SDPData{SDP: sdp})
}

func (s *clientSignal) SendCandidate(cand webrtc.ICECandidateInit) {
	s.hub.sendJSON(s.c, TypeRTCIce, fromICECandidate(cand))
}
