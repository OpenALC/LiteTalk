package signal

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// Hub WS 信令 hub：连接管理、房间登记与广播（docs/落地方案.md §5.3）。
// 与 SFU 的桥接：channel.join 在 WS 层登记成员；PeerConnection 建立由 rtc.* 消息驱动。
type Hub struct {
	sfu       *sfu.SFU
	auth      Authenticator
	log       *log.Logger
	maxPer    int // 房间人数上限（与 SFU 一致）
	chatStore ChatStore
	limits    RateLimiter
	dev       bool // 开发模式：放行非数字的内置房间（生产必须为 false）

	mu    sync.Mutex
	rooms map[string]*roomState
}

// roomState 信令层房间：成员表与语音状态（全内存态，M3 换成频道模型）。
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

	send chan []byte // 下行队列（单写者 goroutine 消费）

	mu     sync.Mutex
	roomID string // 当前所在频道（空 = 未加入）
	state  VoiceStateData
}

// ChatDeps 聊天持久化与频控依赖（可零值：仅广播不落库、不限流）。
type ChatDeps struct {
	Store  ChatStore
	Limits RateLimiter
	// Dev 开发模式：放行非数字的内置房间 ID（M2 遗留场景，无 store 频道实体）。
	// 生产构建（注入了版本号）必须为 false，否则任意字符串房间都可进入。
	Dev bool
}

// NewHub 构造信令 hub。maxMembers <= 0 时取 SFU 的上限。
func NewHub(s *sfu.SFU, auth Authenticator, logger *log.Logger, maxMembers int, chat ChatDeps) *Hub {
	if maxMembers <= 0 {
		maxMembers = s.MaxMembers()
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Hub{
		sfu:       s,
		auth:      auth,
		log:       logger,
		maxPer:    maxMembers,
		chatStore: chat.Store,
		limits:    chat.Limits,
		dev:       chat.Dev,
		rooms:     make(map[string]*roomState),
	}
}

// ServeWS 处理 GET /ws?token=dev-<username>。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	userID, username, err := h.auth(r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	c := &client{
		hub:      h,
		conn:     conn,
		userID:   userID,
		username: username,
		send:     make(chan []byte, 256),
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go c.writeLoop(ctx)
	h.sendJSON(c, TypeHello, HelloData{
		User:     UserBrief{ID: userID, Nickname: username},
		Servers:  []any{},
		Friends:  []any{},
		Presence: map[string]any{},
	})
	h.log.Printf("ws: %s 已连接", userID)

	defer h.cleanup(c)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return // 断连：cleanup 统一收尾
		}
		h.handleMessage(c, data)
	}
}

// writeLoop 唯一写者：保证 coder/websocket 单写者约束。
func (c *client) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			_ = c.conn.Close(websocket.StatusNormalClosure, "server shutdown")
			return
		case msg := <-c.send:
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

// cleanup 断连收尾：离房（触发 SFU 路由清理 + peer_left 广播）。
func (h *Hub) cleanup(c *client) {
	h.leaveRoom(c)
	h.log.Printf("ws: %s 已断开", c.userID)
}

// sendJSON 编码并投递下行消息；队列满则丢弃并记日志（信令低频率，256 缓冲足够）。
func (h *Hub) sendJSON(c *client, msgType string, data any) {
	raw, err := MarshalEnvelope(msgType, data)
	if err != nil {
		h.log.Printf("ws: 编码 %s 失败: %v", msgType, err)
		return
	}
	select {
	case c.send <- raw:
	default:
		h.log.Printf("ws: %s 下行队列满，丢弃 %s", c.userID, msgType)
	}
}

func (h *Hub) sendError(c *client, code, message string) {
	h.sendJSON(c, TypeError, ErrorData{Code: code, Message: message})
}

// handleMessage 分发一条上行信令。
func (h *Hub) handleMessage(c *client, data []byte) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		h.sendError(c, ErrCodeBadMessage, "信封不是合法 JSON")
		return
	}
	switch env.Type {
	case TypeChannelJoin:
		var d ChannelJoinData
		if err := env.Decode(&d); err != nil || d.ChannelID == "" {
			h.sendError(c, ErrCodeBadMessage, "channel.join 需要 channel_id")
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
		h.handleRTCOffer(c, d.SDP)
	case TypeRTCAnswer:
		var d SDPData
		if err := env.Decode(&d); err != nil || d.SDP == "" {
			h.sendError(c, ErrCodeBadMessage, "rtc.answer 需要 sdp")
			return
		}
		h.handleRTCAnswer(c, d.SDP)
	case TypeRTCICE:
		var d ICECandidateData
		if err := env.Decode(&d); err != nil {
			h.sendError(c, ErrCodeBadMessage, "rtc.ice 需要 candidate")
			return
		}
		h.handleRTCCandidate(c, d)
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
	default:
		h.sendError(c, ErrCodeBadMessage, "未知消息类型: "+env.Type)
	}
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
	if _, err := h.chatStore.Member(ctx, ch.ServerID, uid); err != nil {
		return ErrCodeForbidden, "无权进入该频道"
	}
	return "", ""
}

// joinRoom 登记成员并向全房间重发 channel.joined（成员全量，§5.3 无 peer_joined 消息，
// 成员变化以全量下发，客户端做全量替换）。
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
	if c.roomIDLocked() != "" {
		h.mu.Unlock()
		h.leaveRoom(c) // 先离开旧房间再进新房间
		h.mu.Lock()
	}
	room := h.rooms[channelID]
	if room == nil {
		room = &roomState{id: channelID, members: make(map[string]*client)}
		h.rooms[channelID] = room
	}
	if len(room.members) >= h.maxPer {
		h.mu.Unlock()
		h.sendError(c, ErrCodeRoomFull, "语音房间已满")
		return
	}
	room.members[c.userID] = c
	c.setRoomID(channelID)
	members := room.snapshotLocked()
	h.mu.Unlock()

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

// leaveRoom 离房：移除成员、清理 SFU 路由、向剩余成员广播 channel.peer_left。
func (h *Hub) leaveRoom(c *client) {
	h.mu.Lock()
	roomID := c.roomIDLocked()
	if roomID == "" {
		h.mu.Unlock()
		return
	}
	c.setRoomID("")
	room := h.rooms[roomID]
	var remaining []*client
	if room != nil {
		delete(room.members, c.userID)
		if len(room.members) == 0 {
			delete(h.rooms, roomID)
		} else {
			remaining = room.snapshotLocked()
		}
	}
	h.mu.Unlock()

	// SFU 路由清理（移除路由、关闭其 PC）
	h.sfu.Leave(roomID, c.userID)
	h.log.Printf("ws: %s 离开频道 %s", c.userID, roomID)

	for _, m := range remaining {
		h.sendJSON(m, TypeChannelPeerLeft, ChannelPeerLeftData{
			ChannelID: roomID,
			UserID:    c.userID,
		})
	}
}

// handleRTCOffer 初始 rtc.offer（C→S）→ SFU 应答 → rtc.answer（S→C）。
func (h *Hub) handleRTCOffer(c *client, sdp string) {
	roomID := c.roomIDLocked()
	if roomID == "" {
		h.sendError(c, ErrCodeNotInChannel, "请先 channel.join 再发起 rtc.offer")
		return
	}
	answer, err := h.sfu.HandleOffer(roomID, c.userID, sdp, &clientSignal{hub: h, c: c})
	if err != nil {
		code := ErrCodeRTC
		if errors.Is(err, sfu.ErrRoomFull) {
			code = ErrCodeRoomFull
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
func (h *Hub) handleVoiceState(c *client, d VoiceStateData) {
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
	bcast := VoiceStateBroadcast{
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
	s.hub.sendJSON(s.c, TypeRTCICE, ICECandidateData(cand))
}
