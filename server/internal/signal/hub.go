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
	"github.com/OpenALC/LiteTalk/server/internal/authz"
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

	// 优雅关闭（#21）：server.shutdown 的发送窗口、等待 cleanup 的上限与轮询间隔。
	// 超时后打日志继续关闭——不能因为个别连接卡住就拖住进程退出。
	shutdownSendWindow  = 150 * time.Millisecond
	defaultShutdownWait = 5 * time.Second
	shutdownPollEvery   = 20 * time.Millisecond
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
	// authz 权限判定（#35 B3）：与 api.Server 共享实例；nil 时权限门 fail-closed。
	authz Authz

	// 心跳参数（默认值见包常量；测试可缩短）
	heartbeatEvery time.Duration
	pongWait       time.Duration
	readIdle       time.Duration
	presenceTTL    time.Duration // 在线态 TTL（默认 defaultPresenceTTL，测试可缩短）
	shutdownWait   time.Duration // Shutdown 等待 cleanup 的上限（默认 defaultShutdownWait，测试可缩短）

	// shuttingDown 关闭标记（#21）：在 Shutdown 广播/断连之前置位，ServeWS 据此
	// 拒绝新连接，杜绝「广播之后仍有新 WS 接入」的窗口。
	shuttingDown atomic.Bool

	mu    sync.Mutex
	rooms map[string]*roomState
	conns map[connKey]*client // uid+"\x00"+dev → 当前连接（§0.8）
	// userRooms uid → 当前语音房 ID（#92 好友频道 presence 反查）。
	// 只在成员真正写入 room.members[uid] 的同一临界区设置；leaveWorkLocked
	// 核对 members[uid]==c 且映射值等于本次 roomID 才删除，旧连接迟到的
	// cleanup 不得删新连接的映射。同受 h.mu 保护。
	userRooms  map[string]string
	nextConnID uint64 // 连接标识自增（SFU peer 归属核对用）

	// presence 写入串行化（#11）：KV SET/DEL 与好友推送都在 h.mu 之外执行，
	// 但必须按「当前连接事实」依次应用，否则旧连接的 DEL 可能落到新连接的
	// SET 之后。锁序固定 presenceMu → h.mu，禁止反向获取。
	presenceMu        sync.Mutex
	presencePublished map[string]bool // uid → 本进程已发布/已推送的在线态
	// presenceChanPublished 好友频道 presence 的去抖缓存（#92）：uid → 上次推送的
	// 语音房状态。无键 = 尚未推过（离线边沿清空，重进同一频道必须重推）。
	presenceChanPublished map[string]presenceChanState

	// evlog 事件面每会话事件环（#32）：seq 分配与断线续传补发。
	// 实现为进程内环形队列（单实例假设，见 gateway.go 头注释）。
	evlog EventLog

	// 配额与频控（#20）：maxConns 单 uid 连接数；joinRate/offerRate 每分钟每 uid
	// （走 KV 全局限流，重连不清零；limits 为 nil 时回落到单连接计数）。
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
// serverID 来自 ChannelByID（#92）；dev 内置房间无 store 实体为 0，好友频道
// presence 据此不带频道字段。
type roomState struct {
	id       string
	serverID int64
	members  map[string]*client // userID → client
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
	// rate 单连接消息频控窗口（#20）：limits 为 nil 时的回落实现，随连接销毁丢弃。
	rate map[string]*rateWin

	// protoVer 协议版本（#29）：protoVerUnnegotiated=未收 client.hello（按 v1 处理）、
	// protoVerIncompatible=版本无交集（拒收业务消息但不断连，裁定 #9）、≥1=协商结果。
	// 读循环 goroutine 写、任意 goroutine 读 → atomic。
	protoVer atomic.Int64
}

// rateWin 单连接固定窗计数（1 分钟窗，#20；仅 limits 为 nil 时使用）。
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

// Authz 频道权限判定（#35 B3，*authz.Authorizer 满足；测试可注入故障假实现）。
// CanInServer 语义与 authz 包一致：非成员/无权限 (false, nil)，故障 (false, err)。
type Authz interface {
	CanInServer(ctx context.Context, uid, serverID, channelID int64, perm store.Permission) (bool, error)
	// CanServer 服务器级判定（#37 密码旁路：Administrator/owner）。
	CanServer(ctx context.Context, uid, serverID int64, perm store.Permission) (bool, error)
	InvalidateServer(serverID int64)
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
	// JoinRate / OfferRate 单 uid channel.join、rtc.offer 的每分钟次数上限（#20）；0 = 不限。
	JoinRate  int
	OfferRate int
	// Backlog 每会话事件环条数（LITETALK_GATEWAY_BACKLOG，#32）；0 = 默认 256。
	Backlog int
	// Authz 权限判定（#35 B3）。nil 且 Store 是 store.Store 时自建默认实例（测试兼容）；
	// 生产必须注入与 api 共享的同一实例，否则写后失效不同步。
	Authz Authz
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
	az := chat.Authz
	if az == nil {
		// 测试/单用方场景自建（#35 B3）：Store 是 store.Store 才能建；
		// 生产（main.go）注入与 api 共享的实例。
		if st, ok := chat.Store.(store.Store); ok {
			az = authz.New(st, authz.Options{})
		}
	}
	h := &Hub{
		sfu:               s,
		auth:              authn,
		sess:              sess,
		log:               logger,
		maxPer:            maxMembers,
		chatStore:         chat.Store,
		limits:            chat.Limits,
		presence:          chat.Presence,
		dev:               chat.Dev,
		maxConns:          chat.MaxConns,
		joinRate:          chat.JoinRate,
		offerRate:         chat.OfferRate,
		authz:             az,
		heartbeatEvery:    defaultHeartbeatEvery,
		pongWait:          defaultPongWait,
		readIdle:          defaultReadIdle,
		rooms:             make(map[string]*roomState),
		conns:             make(map[connKey]*client),
		userRooms:         make(map[string]string),
		evlog:             newMemEventLog(chat.Backlog),
		presencePublished: make(map[string]bool),
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
	// 关闭中拒绝新连接（#21）：必须排在鉴权/注册之前，否则握手会绕过 Shutdown
	// 的连接快照，连接既不收 server.shutdown 也不被关闭。
	if h.shuttingDown.Load() {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "shutting_down",
			"message": "服务正在关闭",
		})
		return
	}
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
	// becameOnline 是 register 在 h.mu 内判定的 0→1 边沿（#11）：只有真正
	// 从零到一的那次连接触发 presence 写入与上线推送。
	old, becameOnline := h.register(c)
	// 事件环登记（#32）：会话（sid）订阅随首次连接建立，断线后环保留供 resume 补发。
	h.evlog.Bind(c.sid, c.userID)
	go c.writeLoop(ctx)
	go h.heartbeatLoop(ctx, c)
	if old != nil {
		h.replace(old, c)
	}
	if becameOnline {
		h.syncPresence(c.userID)
	}

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

// register 登记连接并分配 connID；返回同 uid+dev 的旧连接（nil=无），以及
// 该 uid 是否由 0 条连接变为 1 条（becameOnline，#11 的上线边沿）。
// 边沿判定必须与 conns 变更处于同一临界区：放到锁外数连接数时，多设备
// 同时首连会各自看到「已有连接」而无人推送上线。
func (h *Hub) register(c *client) (old *client, becameOnline bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextConnID++
	c.connID = h.nextConnID
	key := makeConnKey(c.userID, c.dev)
	old = h.conns[key]
	// 边沿看插入前是否有任何设备连接：同设备顶替时槽位仍被旧连接占着，
	// 插入后数连接数会得到 1，误判成 0→1（P2 重复推送 online）。
	hadAny := h.connCountLocked(c.userID) > 0
	h.conns[key] = c
	return old, !hadAny
}

// unregister 仅当 conns[key] 仍是本连接时摘除（被顶替的旧连接不得误删新连接，§0.8）。
// 返回该 uid 是否由 1 条连接变为 0 条（becameOffline，#11 的下线边沿）；
// 与删除在同一临界区判定，避免「先查后删」race。
func (h *Hub) unregister(c *client) (becameOffline bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := makeConnKey(c.userID, c.dev)
	if h.conns[key] != c {
		return false // 已被顶替：新连接仍在同一 uid 下，状态未变
	}
	delete(h.conns, key)
	return h.connCountLocked(c.userID) == 0
}

// connCountLocked 调用方须持 h.mu。
func (h *Hub) connCountLocked(uid string) int {
	prefix := uid + "\x00"
	n := 0
	for k := range h.conns {
		if strings.HasPrefix(string(k), prefix) {
			n++
		}
	}
	return n
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

// Shutdown 优雅关闭（#21）：先置关闭标记再广播 server.shutdown，发送窗口后
// CloseNow 全部连接，并等待 conns 清零（各 WS 处理器已进入 cleanup 的 unregister；
// 其后的离房与 presence 摘除在 cleanup 内继续执行）。等待有上限（默认 5s，测试
// 可缩短）：超时打日志后返回，不阻塞进程退出。幂等：重复/并发调用只有首个调用者
// 执行关闭流程。
//
// 与 main 的关闭序兼容：本函数返回后再关 HTTP，此时 WS 已全部断开。
func (h *Hub) Shutdown(reason string) {
	if reason == "" {
		reason = "shutdown"
	}
	// 标记必须在取连接快照之前置位：先广播再置位的话，广播与断连之间完成握手的
	// 新连接会错过 server.shutdown（复审 #21 指出的窗口）。
	if !h.shuttingDown.CompareAndSwap(false, true) {
		return
	}
	wait := h.shutdownWait
	if wait <= 0 {
		wait = defaultShutdownWait
	}
	deadline := time.Now().Add(wait)

	// 分批关闭：ServeWS 的关闭检查与 register 之间仍有在途握手的残余窗口，
	// 这类连接可能落在首轮快照之外。每轮把新出现的连接一并关闭，直到 conns
	// 清零或超时；等待期间不持 h.mu，cleanup 的 unregister 才能拿到锁。
	closed := make(map[*client]struct{})
	for {
		h.mu.Lock()
		fresh := make([]*client, 0, len(h.conns))
		for _, c := range h.conns {
			if _, ok := closed[c]; !ok {
				fresh = append(fresh, c)
			}
		}
		h.mu.Unlock()

		for _, c := range fresh {
			h.sendEvent(c, TypeServerShutdown, ServerShutdownData{Reason: reason, TS: time.Now().UnixMilli()})
			closed[c] = struct{}{}
		}
		if len(fresh) > 0 {
			// 发送窗口：让下行队列把 shutdown 消息写出去（锁外睡眠）。
			if rem := time.Until(deadline); rem > 0 {
				time.Sleep(min(rem, shutdownSendWindow))
			}
			for _, c := range fresh {
				c.closeNow()
			}
		}

		n := h.waitConnsDrained(deadline)
		if n == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			h.log.Printf("ws: 关闭等待超时（上限 %v），仍有 %d 条连接未完成 cleanup，继续关闭", wait, n)
			return
		}
	}
}

// waitConnsDrained 轮询等待 conns 清零，返回剩余连接数。每次轮询单独持锁，
// 睡眠不持 h.mu。
func (h *Hub) waitConnsDrained(deadline time.Time) int {
	for {
		h.mu.Lock()
		n := len(h.conns)
		h.mu.Unlock()
		if n == 0 || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(shutdownPollEvery)
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
	// 好友频道 presence（#92）：被真正摘除的 uid 逐个收口（离房后在线-only）。
	synced := make(map[string]bool, len(removed))
	for _, w := range pending {
		if !w.removed || synced[w.c.userID] {
			continue
		}
		synced[w.c.userID] = true
		h.syncPresenceChannel(w.c.userID)
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
// unregister 判定出 1→0 边沿时下线 presence。均做身份核对（conns[key]==c、
// members[uid]==c），被顶替的旧连接不会误删新连接（§0.8）。多处调用
// （defer/心跳超时/写失败），全程幂等。
func (h *Hub) cleanup(c *client) {
	becameOffline := h.unregister(c)
	h.leaveRoom(c)
	if becameOffline {
		h.syncPresence(c.userID)
	}
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
		// join 频控（#20）：单 uid 每分钟上限（跨连接累计），超限 rate_limited。
		if !h.allowRate(c, TypeChannelJoin, h.joinRate) {
			h.sendError(c, ErrCodeRateLimited, "入房过于频繁")
			return
		}
		if code, msg := h.authorizeChannelJoin(c, d.ChannelID); code != "" {
			h.sendError(c, code, msg)
			return
		}
		h.joinRoom(c, d.ChannelID, d.Password)
	case TypeChannelLeave:
		h.leaveRoom(c)
	case TypeRTCOffer:
		var d SDPData
		if err := env.Decode(&d); err != nil || d.SDP == "" {
			h.sendError(c, ErrCodeBadMessage, "rtc.offer 需要 sdp")
			return
		}
		// offer 频控（#20）：单 uid 每分钟上限（跨连接累计），超限 rate_limited。
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
// 数字频道要求存在、Type 为 voice、请求者是其所属服务器成员，且有 ViewChannel+
// ConnectVoice（#35 B3）；非数字房间是 M2 内置房间（无 store 频道实体），仅 dev 模式放行。
// 权限检查放在既有检查之后，保持既有错误优先级：不存在→not_found、文本频道→forbidden、
// 非成员→forbidden，然后才是权限不足。
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
	// B3 权限：进语音房 = 看得见频道 + 允许连线；无权限 forbidden，故障 internal。
	for _, perm := range []store.Permission{store.ViewChannel, store.ConnectVoice} {
		code, msg := h.checkPerm(ctx, uid, ch.ServerID, chID, perm, "无权进入该频道")
		if code != "" {
			return code, msg
		}
	}
	// 缓存 tag 到连接上（成员快照填充用；join 是低频路径，一次查询即可）
	c.mu.Lock()
	c.tag = mem.Tag
	c.mu.Unlock()
	return "", ""
}

// checkPerm 单权限判定（#35 B3）：无权限 → (forbidden, msg)，查询故障 → (internal, …)，
// 通过 → ("", "")。authz 未注入同样按 internal 拒绝（fail-closed，不做假放行）。
func (h *Hub) checkPerm(ctx context.Context, uid, serverID, channelID int64, perm store.Permission, denyMsg string) (code, msg string) {
	if h.authz == nil {
		return ErrCodeInternal, "权限服务未就绪"
	}
	ok, err := h.authz.CanInServer(ctx, uid, serverID, channelID, perm)
	if err != nil {
		// 故障只记日志 + internal：不能把 500 伪装成 403。
		h.log.Printf("ws: 权限查询失败（用户 %d 频道 %d）: %v", uid, channelID, err)
		return ErrCodeInternal, "权限查询失败"
	}
	if !ok {
		return ErrCodeForbidden, denyMsg
	}
	return "", ""
}

// joinRoom 登记成员并向全房间重发 channel.joined（成员全量，§5.3 无 peer_joined 消息，
// 成员变化以全量下发，客户端做全量替换）。整个登记在一把 hub.mu 内原子完成。
// #37 顺序（joinRoom 是密码/人数的唯一权威入口）：结构/权限 → 已在目标房 →
// 密码（管理员旁路）→ 人数上限 → 离开旧房 → 登记。密码校验不持 hub.mu
// （bcrypt 贵，不能让整个 hub 排队）；人数检查在 h.mu 内且先于离开旧房——
// 满员被拒不得改变原房间成员关系。语音单处（§0.10）：同 uid 其他会话在任一
// 语音房 → 先发 session.voice_taken、清其房间成员与 SFU peer，然后本连接进房
// （自己另一设备已在目标房时走此路，容量检查对它放行顶替）。
func (h *Hub) joinRoom(c *client, channelID, password string) {
	// 兜底再校验一次：joinRoom 可能被 channel.join 之外的路径调用，权限不可绕。
	if code, msg := h.authorizeChannelJoin(c, channelID); code != "" {
		h.sendError(c, code, msg)
		return
	}
	// 数字频道取频道行（密码/人数上限）；dev 内置房间无 store 实体，跳过两道门。
	// authorize 已确认存在，此刻再查失败按 not_found（竞态删频道）。
	var ch *store.Channel
	if chID, err := strconv.ParseInt(channelID, 10, 64); err == nil && h.chatStore != nil {
		c2, err := h.chatStore.ChannelByID(context.Background(), chID)
		if err != nil {
			h.sendError(c, ErrCodeNotFound, "频道不存在")
			return
		}
		ch = c2
	}
	// roomState.serverID（#92）：dev 内置房间保持 0，好友频道 presence 不带字段。
	var roomServerID int64
	if ch != nil {
		roomServerID = ch.ServerID
	}
	uid, uidErr := strconv.ParseInt(c.userID, 10, 64)

	// 已在目标房（#37 顺序在密码之前：重发 join 不应被密码门拦）。
	h.mu.Lock()
	already := c.roomIDLocked() == channelID
	h.mu.Unlock()
	if already {
		h.sendError(c, ErrCodeAlreadyInChan, "已在该频道中")
		return
	}

	// 密码（#37）：频道未设密码时忽略携带值；管理员（Administrator/owner）旁路。
	// 没带密码 → forbidden（老客户端安全回滚）；带错 → bad_password。
	if ch != nil && ch.PasswordHash != "" {
		if uidErr != nil {
			h.sendError(c, ErrCodeForbidden, "无权进入该频道")
			return
		}
		bypass := false
		if h.authz != nil {
			var aerr error
			bypass, aerr = h.authz.CanServer(context.Background(), uid, ch.ServerID, store.Administrator)
			if aerr != nil {
				h.log.Printf("ws: 密码旁路查询失败（用户 %d 频道 %d）: %v", uid, ch.ID, aerr)
				h.sendError(c, ErrCodeInternal, "权限查询失败")
				return
			}
		}
		if !bypass {
			if password == "" {
				h.sendError(c, ErrCodeForbidden, "该频道需要密码")
				return
			}
			if !auth.CheckPassword(ch.PasswordHash, password) {
				h.sendError(c, ErrCodeBadPassword, "频道密码错误")
				return
			}
		}
	}

	h.mu.Lock()
	if c.roomIDLocked() == channelID {
		h.mu.Unlock()
		h.sendError(c, ErrCodeAlreadyInChan, "已在该频道中")
		return
	}
	// 人数上限（#37）：频道限与全局限取小；自己另一设备已在目标房可顶替腾位。
	// 在离开旧房之前判（满员被拒不得改变原房间成员关系），且不留空房壳。
	limit := h.maxPer
	if ch != nil && ch.UserLimit > 0 && ch.UserLimit < limit {
		limit = ch.UserLimit
	}
	if room := h.rooms[channelID]; room != nil {
		occ := len(room.members)
		if other := room.members[c.userID]; other != nil && other != c {
			occ-- // 同 uid 另一设备在目标房：语音单处会顶替它，提前腾位
		}
		if occ >= limit {
			h.mu.Unlock()
			// 容量超限统一 quota_exceeded（§0.1.3），文案可区分。
			h.sendError(c, ErrCodeQuotaExceeded, "语音房间已满")
			return
		}
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
		room = &roomState{id: channelID, serverID: roomServerID, members: make(map[string]*client)}
		h.rooms[channelID] = room
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
		room = &roomState{id: channelID, serverID: roomServerID, members: make(map[string]*client)}
		h.rooms[channelID] = room
	}
	room.members[c.userID] = c
	c.setRoomID(channelID)
	// 频道反查（#92）：与成员登记同一临界区写入，避免读侧看到「在房但无映射」。
	if h.userRooms == nil {
		h.userRooms = make(map[string]string)
	}
	h.userRooms[c.userID] = channelID
	members := room.snapshotLocked()
	h.mu.Unlock()

	// 锁外：被抢占/自旧房的 SFU 归属清理 + peer_left 广播（peer_left 先于 joined，
	// 同房抢占时接收方按序应用后成员列表仍含本 uid）。
	for _, w := range pending {
		h.finishLeave(w)
	}

	h.log.Printf("ws: %s 加入频道 %s（%d 人）", c.userID, channelID, len(members))
	all := memberInfos(members)
	// 在线态从 presence KV 读取（不持 h.mu，逐成员一次 GET）；Self 取同一份快照。
	byUID := make(map[string]MemberInfo, len(all))
	for i := range all {
		all[i].Online = h.presenceOnline(all[i].UserID)
		byUID[all[i].UserID] = all[i]
	}
	// #46 音质配置：语音频道随 joined 下发权威值（0 = 跟随设置/默认 20ms）。
	// dev 内置房间没有 store 实体（ch == nil），字段缺省，客户端按默认处理。
	var audioBitrate, audioFrame *int64
	if ch != nil {
		b, f := int64(ch.AudioBitrateKbps), int64(ch.AudioFrameMS)
		audioBitrate, audioFrame = &b, &f
	}
	for _, m := range members {
		h.sendJSON(m, TypeChannelJoined, ChannelJoinedData{
			ChannelID:        channelID,
			Members:          all,
			Self:             byUID[m.userID],
			AudioBitrateKbps: audioBitrate,
			AudioFrameMs:     audioFrame,
		})
	}
	// 好友频道 presence（#92）：登记完成才是状态变化点。中途 leaveWork 不单独推，
	// 否则切房会先推一条「无频道」再推目标房；失败路径都 return 在登记之前。
	// 同 uid 被 voice_taken 的旧连接也是同一 uid 的状态迁移，由这里一次收口。
	h.syncPresenceChannel(c.userID)
}

// leaveWork 一次离房的锁内计算结果：锁外执行 SFU 清理与广播。
type leaveWork struct {
	c         *client
	roomID    string
	removed   bool // 本连接确实是该房成员（需要广播 peer_left）
	remaining []*client
}

// leaveWorkLocked 离房（调用方须持 h.mu）：清本连接 roomID、身份核对后摘成员。
// members[uid] 不是本连接（已被同账号新连接顶替）时不摘成员、不广播（§0.8 盲删防护）；
// userRooms 同理——旧连接迟到的 cleanup 不得删掉新连接写入的映射（#92）。
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
		if h.userRooms[c.userID] == roomID {
			delete(h.userRooms, c.userID)
		}
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
// 离房成功后收口好友频道 presence（#92）；joinRoom 的中途离房不走本函数，
// 状态变化由 join 成功后的那次 sync 一次收口（见 joinRoom 尾注）。
func (h *Hub) leaveRoom(c *client) {
	h.mu.Lock()
	w := h.leaveWorkLocked(c)
	h.mu.Unlock()
	if w != nil {
		h.finishLeave(*w)
		h.syncPresenceChannel(c.userID)
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
// server_muted 是服务端权威字段（#36）：上行携带一律忽略，只有 SetServerMuted
// 的广播才带——否则任意客户端可伪造「被闭麦/已解禁」状态。
func (h *Hub) handleVoiceState(c *client, d VoiceStateData) {
	if c.protoVer.Load() >= 2 {
		d.Speaking = nil
	}
	d.ServerMuted = nil
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

// SetServerMuted 服务端强制闭麦/解禁（#36）：SFU 包级丢弃（track 不摘）并向房间
// 全体广播 voice.state{user_id, server_muted}（目标本人也收，用于状态显示）。
// 目标不在该频道 → ErrNotInChannel（REST 层映射 404）。与 voice.request_state 的
// 「请求」语义不同：这里不看目标是否配合，服务端直接生效。
func (h *Hub) SetServerMuted(channelID, userID string, muted bool) error {
	h.mu.Lock()
	room := h.rooms[channelID]
	if room == nil || room.members[userID] == nil {
		h.mu.Unlock()
		return ErrNotInChannel
	}
	// 快照成员后放锁再动 SFU/发下行：SFU 侧有自己的锁序（s.mu → r.mu），
	// 持 h.mu 交叉调用会与 speakingLoop 的回调路径死锁（见 tickRooms 注释）。
	targets := make([]*client, 0, len(room.members))
	for _, m := range room.members {
		targets = append(targets, m)
	}
	h.mu.Unlock()

	if !h.sfu.SetServerMuted(channelID, userID, muted) {
		return ErrNotInChannel
	}
	bcast := VoiceStateData{UserID: userID, ServerMuted: &muted}
	for _, m := range targets {
		h.sendJSON(m, TypeVoiceState, bcast)
	}
	h.log.Printf("ws: 频道 %s 成员 %s 服务端闭麦=%v（%d 人已通知）", channelID, userID, muted, len(targets))
	return nil
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

// VoiceChannelOf 反查 uid 当前语音房（#92 好友频道 presence，api.VoicePresence 满足）。
// serverID 为十进制字符串；dev 内置房间无 store 实体为 "0"，调用方据此不带频道字段。
// 只读内存映射，不查 store、不读 presence KV。持 h.mu。
func (h *Hub) VoiceChannelOf(uid string) (channelID, serverID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.userRooms[uid]
	if !ok {
		return "", "", false
	}
	room, ok := h.rooms[ch]
	if !ok {
		return "", "", false // 映射与房间表不一致：防御性视为不在房
	}
	return ch, formatUID(room.serverID), true
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
	return h.connCountLocked(uid)
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

// rateWindow 消息频控窗长（固定窗 1 分钟，#20）。
const rateWindow = time.Minute

// allowRate 消息频控（#20）：join/offer 按 uid、按消息类型限流，键 ws:<uid>:<msgType>，
// 与 chat 的 msg:<uid> 同一 KV 通道。按 uid 而非按连接是必须的——连接级计数在重连/
// 多设备之间各自归零，等于没有上限；KV 窗口（Mem/Redis）跨连接累计。
// 调用点在 WS 读循环，Allow 是一次本地 Mem 或单次 Redis 调用，与 chat 频控同路径，
// 不引入额外阻塞。h.limits 为 nil（测试/过渡）时回落到单连接计数，此时重连会清零。
// limit<=0 不限流；超限或 KV 出错返回 false（fail-closed，与 chat 一致）。
func (h *Hub) allowRate(c *client, msgType string, limit int) bool {
	if limit <= 0 {
		return true
	}
	if h.limits != nil {
		ok, err := h.limits.Allow(context.Background(), "ws:"+c.userID+":"+msgType, limit, rateWindow)
		return err == nil && ok
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
		infos = append(infos, memberInfoOf(m))
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
