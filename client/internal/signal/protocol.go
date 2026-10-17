// Package signal 实现轻音 LiteTalk 客户端的 WebSocket 信令层。
//
// 协议契约见 docs/落地方案.md §5.3：信封 {type, data, ts}，
// 服务端持有同一份小定义（双端各持一份，可接受重复）。
package signal

import (
	"encoding/json"
	"time"

	"github.com/coder/websocket"
)

// nowMillis 当前 Unix 毫秒（信封 ts）。
func nowMillis() int64 { return time.Now().UnixMilli() }

// 信令消息类型（与服务端逐字一致）。
const (
	TypeHello           = "hello"             // S→C 鉴权成功后下发全量初始数据
	TypeChannelJoin     = "channel.join"      // C→S 加入语音频道
	TypeChannelJoined   = "channel.joined"    // S→C 房间成员全量
	TypeChannelLeave    = "channel.leave"     // C→S 离开频道
	TypeChannelPeerLeft = "channel.peer_left" // S→C 有成员离开
	TypeRTCOffer        = "rtc.offer"         // 双向 SDP offer
	TypeRTCAnswer       = "rtc.answer"        // 双向 SDP answer
	TypeRTCIce          = "rtc.ice"           // 双向 trickle ICE
	TypeVoiceState      = "voice.state"       // C→S→广播 说话/静音/耳聋
	// v0.1.2 请求静音：C→S voice.request_state，S→C 定向 voice.state_request。
	TypeVoiceRequestState = "voice.request_state"
	TypeVoiceStateRequest = "voice.state_request"
	TypeChatSend          = "chat.send"       // C→S
	TypeChatMessage       = "chat.message"    // S→C
	TypePresenceUpdate    = "presence.update" // S→C
	TypeFriendRequest     = "friend.request"  // S→C
	TypeFriendAccept      = "friend.accept"   // S→C
	TypeError             = "error"           // S→C
	// 批次 2 多设备会话（契约 §0.5，只增不改；与 server/internal/signal/protocol.go 同步）。
	TypeSessionKicked     = "session.kicked"      // S→C 同类设备新登录踢线，随后 close 4001
	TypeSessionVoiceTaken = "session.voice_taken" // S→C 同账号另一会话抢占语音，本端本地退房
)

// WS close code（契约 §0.6，4000~4999 为应用自定义区间；与
// server/internal/signal/protocol.go 逐字同步）。
const (
	// CloseCodeSessionReplaced 4000：同会话（同 sid）半开连接被新连接替换——
	// 静默关闭，不发 session.kicked（客户端停止重连，服务端已有新连接）。
	CloseCodeSessionReplaced = websocket.StatusCode(4000)
	// CloseCodeNewLogin 4001：同类设备新登录（或登出/重置密码踢线）——
	// 旧连接先收 session.kicked，300~500ms 发送窗口后以此码关闭。
	CloseCodeNewLogin = websocket.StatusCode(4001)
)

// KickReasonLoginElsewhere session.kicked.reason 的标准取值（同类设备新登录）。
const KickReasonLoginElsewhere = "login_elsewhere"

// 下行 error 消息的 code 取值（与 server/internal/signal/protocol.go 对齐）。
const (
	ErrCodeBadMessage    = "bad_message"
	ErrCodeNotInChannel  = "not_in_channel"
	ErrCodeAlreadyInChan = "already_in_channel"
	ErrCodeRoomFull      = "room_full"
	ErrCodeRTC           = "rtc_error"
	ErrCodeInternal      = "internal"
	ErrCodeForbidden     = "forbidden"
	ErrCodeNotFound      = "not_found"
	ErrCodeRateLimited   = "rate_limited"
)

// Envelope 信令信封。Data 延迟解析，由订阅者按 Type 自行解码。
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
	TS   int64           `json:"ts"`
}

// NewEnvelope 打包一条待发消息（ts 取当前 Unix 毫秒）。
func NewEnvelope(msgType string, data any) (Envelope, error) {
	env := Envelope{Type: msgType, TS: nowMillis()}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return Envelope{}, err
		}
		env.Data = raw
	}
	return env, nil
}

// Decode 解析 Data 到 v（Data 为空时返回 nil 错误，v 保持零值）。
func (e Envelope) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// ---- 各消息 data 载荷 ----

// UserBrief hello.user 子对象（与服务端 wire 对齐：id + nickname）。
type UserBrief struct {
	ID       string `json:"id"`
	Nickname string `json:"nickname"`
}

// HelloData hello 载荷：鉴权成功后下发的全量初始数据（M3 前 servers/friends 恒空）。
type HelloData struct {
	User     UserBrief      `json:"user"`
	Servers  []any          `json:"servers"`
	Friends  []any          `json:"friends"`
	Presence map[string]any `json:"presence"`
}

// ChatMessageData chat.message 载荷。
type ChatMessageData struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	Content   string    `json:"content"`
	TS        int64     `json:"ts"`
	User      UserBrief `json:"user"`
}

// SDPData 是 rtc.offer / rtc.answer 的载荷。
type SDPData struct {
	SDP string `json:"sdp"`
}

// ICEData 是 rtc.ice 的载荷，字段与 WebRTC ICECandidateInit / pion 对齐。
type ICEData struct {
	Candidate        string  `json:"candidate"`
	SDPMid           string  `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment string  `json:"usernameFragment,omitempty"`
}

// ChannelJoinData 是 channel.join / channel.leave 的载荷。
type ChannelJoinData struct {
	ChannelID string `json:"channel_id"`
}

// Member 语音房间成员（channel.joined 全量项）。
type Member struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Tag      string `json:"tag,omitempty"` // 昵称#tag 的 tag，好友请求用
	Speaking bool   `json:"speaking"`
	Mute     bool   `json:"mute"`
	Deafen   bool   `json:"deafen"`
}

// ChannelJoinedData 是 channel.joined 的载荷（房间成员全量 + 自身）。
type ChannelJoinedData struct {
	ChannelID string   `json:"channel_id"`
	Members   []Member `json:"members"`
	Self      Member   `json:"self"`
}

// PeerLeftData 是 channel.peer_left 的载荷。
type PeerLeftData struct {
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
}

// VoiceStateData 是 voice.state 的载荷。
// 上行（C→S）不带 UserID；下行广播（S→C）由服务端补上 UserID 标识来源。
// 三个状态字段均为可选增量：指针 nil 表示该字段本次未变。
type VoiceStateData struct {
	UserID   string `json:"user_id,omitempty"`
	Speaking *bool  `json:"speaking,omitempty"`
	Mute     *bool  `json:"mute,omitempty"`
	Deafen   *bool  `json:"deafen,omitempty"`
}

// VoiceRequestStateData C→S voice.request_state：请求目标闭麦/静音（v0.1.2）。
type VoiceRequestStateData struct {
	ChannelID string `json:"channel_id"`
	TargetID  string `json:"target_id"`
	Mute      *bool  `json:"mute,omitempty"`
	Deafen    *bool  `json:"deafen,omitempty"`
}

// VoiceStateRequestData S→C voice.state_request：他人对你发出的闭麦/静音请求。
type VoiceStateRequestData struct {
	From   UserBrief `json:"from"`
	Mute   *bool     `json:"mute,omitempty"`
	Deafen *bool     `json:"deafen,omitempty"`
	TS     int64     `json:"ts"`
}

// ErrorData 是 error 消息的载荷。
// Code 为字符串枚举（bad_message / unauthorized / ...），与 server 对齐（审查 #3）。
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SessionKickedData 是 session.kicked 的载荷（契约 §0.5）。
// 本期 reason 恒为 "login_elsewhere"（同类设备新登录），device_type 为接管端设备。
// session.voice_taken 载荷为空对象，无需解码。
type SessionKickedData struct {
	Reason     string `json:"reason"`
	DeviceType string `json:"device_type"`
}

// Bool 返回 b 的指针（构造可选增量字段用）。
func Bool(b bool) *bool { return &b }
