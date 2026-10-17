// Package signal 实现 LiteTalk WS 信令层。
// 协议契约见 docs/落地方案.md §5.3：JSON 信封 {type, data, ts}。
// M2 阶段：房间与成员全部内存态；开发令牌 dev-<username>，JWT 校验留 M3 接入。
package signal

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// 信令消息类型（§5.3）
const (
	TypeHello           = "hello"
	TypeChannelJoin     = "channel.join"
	TypeChannelJoined   = "channel.joined"
	TypeChannelLeave    = "channel.leave"
	TypeChannelPeerLeft = "channel.peer_left"
	TypeRTCOffer        = "rtc.offer"
	TypeRTCAnswer       = "rtc.answer"
	TypeRTCICE          = "rtc.ice"
	TypeVoiceState      = "voice.state"
	// voice.request_state / voice.state_request：请求对方闭麦/静音（v0.1.2）。
	// 定向转发、无权限强制——接收方自行决定，同 Discord「请求静音」语义。
	TypeVoiceRequestState = "voice.request_state" // C→S 请求目标闭麦/静音
	TypeVoiceStateRequest = "voice.state_request" // S→C 定向投递请求
	TypeChatSend          = "chat.send"           // C→S 文字消息发送
	TypeChatMessage       = "chat.message"        // S→C 文字消息广播
	// 多设备会话（v1.1.0，§0 协议契约）：
	// session.kicked：同类设备新登录，旧连接被踢——踢前尽力下发，随后 close 4001。
	TypeSessionKicked = "session.kicked"
	// session.voice_taken：同账号另一会话抢占语音，本端按本地退房处理（不关连接）。
	TypeSessionVoiceTaken = "session.voice_taken"
	TypeError             = "error"
)

// WS close code（§0.6，4000~4999 为应用自定义区间）。
const (
	// CloseCodeSessionReplaced 4000：同会话（同 sid）半开连接被新连接替换——
	// 静默关闭，不发 session.kicked（客户端按正常退出处理，停止重连）。
	CloseCodeSessionReplaced = websocket.StatusCode(4000)
	// CloseCodeNewLogin 4001：同类设备新登录（或登出/重置密码踢线）——
	// 旧连接先收 session.kicked，300~500ms 发送窗口后以此码关闭。
	CloseCodeNewLogin = websocket.StatusCode(4001)
)

// KickReasonLoginElsewhere session.kicked.reason 的标准取值（同类设备新登录）。
const KickReasonLoginElsewhere = "login_elsewhere"

// SessionKickedData S→C session.kicked：踢前尽力下发；随后 close 4001。
type SessionKickedData struct {
	Reason     string `json:"reason"`      // login_elsewhere 等
	DeviceType string `json:"device_type"` // 被踢连接自身的设备类型
}

// 下行 error 消息的 code 取值
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

// Envelope 信令信封：{ "type": "...", "data": {...}, "ts": 1734500000000 }
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
	TS   int64           `json:"ts"`
}

// MarshalEnvelope 打包一条下行消息，ts 取当前毫秒时间戳。
func MarshalEnvelope(msgType string, data any) ([]byte, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{Type: msgType, Data: raw, TS: time.Now().UnixMilli()})
}

// Decode 将信封 data 反序列化到 v。
func (e Envelope) Decode(v any) error {
	if len(e.Data) == 0 {
		return errors.New("信令缺少 data 字段")
	}
	return json.Unmarshal(e.Data, v)
}

// ---- 各消息 data 结构（§5.3 表） ----

// UserBrief 用户简要信息。M2 中 id 即用户名。
// nickname 与客户端 Member 结构对齐（wire 契约）。
type UserBrief struct {
	ID       string `json:"id"`
	Nickname string `json:"nickname"`
}

// HelloData S→C hello：鉴权成功后下发的全量初始数据。
// servers/friends/presence 为 M3 占位，M2 恒为空。
type HelloData struct {
	User     UserBrief      `json:"user"`
	Servers  []any          `json:"servers"`
	Friends  []any          `json:"friends"`
	Presence map[string]any `json:"presence"`
}

// ChannelJoinData C→S channel.join。
type ChannelJoinData struct {
	ChannelID string `json:"channel_id"`
}

// MemberInfo 房间成员快照（含语音状态，供晚加入者拿到当前状态）。
// nickname 字段名与客户端 Member 对齐（wire 契约，审查 #1）。
type MemberInfo struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Mute     bool   `json:"mute"`
	Deafen   bool   `json:"deafen"`
	Speaking bool   `json:"speaking"`
}

// ChannelJoinedData S→C channel.joined：房间成员全量。
// 成员变化时向房间内每个成员重发全量（Self 为接收者本人的 Member 快照，
// 与客户端结构对齐——审查 #2）。
type ChannelJoinedData struct {
	ChannelID string       `json:"channel_id"`
	Members   []MemberInfo `json:"members"`
	Self      MemberInfo   `json:"self"`
}

// ChannelLeaveData C→S channel.leave。
type ChannelLeaveData struct {
	ChannelID string `json:"channel_id"`
}

// ChannelPeerLeftData S→C channel.peer_left。
type ChannelPeerLeftData struct {
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
}

// SDPData rtc.offer / rtc.answer 的 data。
type SDPData struct {
	SDP string `json:"sdp"`
}

// ICECandidateData rtc.ice 的 data，沿用 WebRTC 标准字段命名。
type ICECandidateData = webrtc.ICECandidateInit

// VoiceStateData C→S voice.state：说话/静音/耳聋状态，字段均可选。
type VoiceStateData struct {
	Speaking *bool `json:"speaking,omitempty"`
	Mute     *bool `json:"mute,omitempty"`
	Deafen   *bool `json:"deafen,omitempty"`
}

// VoiceRequestStateData C→S voice.request_state：请求目标成员闭麦/静音。
// 仅「请求」语义——服务端定向转发给目标，由目标自行决定是否执行（无权限强制）。
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

// ChatSendData C→S chat.send：向频道发一条文字消息。
// Type 为消息种类（§0.12）：本期仅支持 "text"，其他值拒收。
type ChatSendData struct {
	ChannelID string `json:"channel_id"`
	Type      string `json:"type"`
	Content   string `json:"content"`
}

// ChatMessageData S→C chat.message：文字消息广播。
// 字段与客户端 client/internal/signal/protocol.go 的 ChatMessageData 逐一对齐。
type ChatMessageData struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	Content   string    `json:"content"`
	TS        int64     `json:"ts"`
	User      UserBrief `json:"user"`
}

// VoiceStateBroadcast S→其他成员 的 voice.state 广播，服务端补充 user_id。
type VoiceStateBroadcast struct {
	UserID   string `json:"user_id"`
	Speaking *bool  `json:"speaking,omitempty"`
	Mute     *bool  `json:"mute,omitempty"`
	Deafen   *bool  `json:"deafen,omitempty"`
}

// ErrorData S→C error。
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---- 鉴权 ----

// ErrUnauthorized 令牌为空或无法提取用户名。
var ErrUnauthorized = errors.New("令牌无效")

// Identity 鉴权结果：身份 + 会话声明（多设备会话 v1.1.0）。
type Identity struct {
	UserID   string
	Username string
	// Sid 会话 ID（32 hex）；空 = 旧版本令牌（无会话约束，sess 存在时按过渡期规则处理）。
	Sid string
	// Dev 设备类型（desktop/mobile/…）；空 = 旧版本令牌。
	Dev string
	// IssuedAt 令牌签发时间（Unix 秒）；旧版本令牌过渡期判定用（v1.1.0 兼容窗口）。
	IssuedAt int64
	// SkipSessionCheck 跳过握手 sid 比对。仅 devAuth 模式的 dev- 令牌置位
	//（dev 令牌无会话概念；生产构建拒绝启用 devAuth，见 cmd/server）。
	SkipSessionCheck bool
}

// Authenticator 校验令牌并返回身份与会话声明。
type Authenticator func(token string) (Identity, error)

// DevAuthenticator M2 开发令牌：dev-<username>。
// 仅做非空校验 + 提取用户名，不做任何签名校验；
// SkipSessionCheck 置位——仅限 devAuth 模式接线（生产构建禁止该模式）。
func DevAuthenticator(token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrUnauthorized
	}
	name := strings.TrimPrefix(token, "dev-")
	if name == "" {
		return Identity{}, ErrUnauthorized
	}
	return Identity{
		UserID:           name,
		Username:         name,
		Dev:              "desktop",
		SkipSessionCheck: true,
	}, nil
}
