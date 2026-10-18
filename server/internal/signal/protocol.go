// Package signal 实现 LiteTalk WS 信令层。
//
// 协议契约的唯一事实来源是 api/signal/（JSON Schema）：消息类型常量、载荷结构、
// 错误码、close code、协议版本都由 server/api/tools/signalgen 生成进 protocol_gen.go，
// 两侧（server/client）生成物字节相同。改契约请改 api/signal/ 并重新生成。
//
// 本文件只保留**手写运行时层**（生成器不管的职责）：
//   - 信封 Envelope / MarshalEnvelope / Decode
//   - 鉴权 Identity / Authenticator / DevAuthenticator / ErrUnauthorized（服务端专属）
//   - ICE 与 pion 的互操作（生成物 ICEData 不 import pion）
//
// 禁止把上述内容塞进 schema；禁止手改 protocol_gen.go。
package signal

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// Envelope 信令信封：{ "type": "...", "data": {...}, "ts": 1734500000000 }。
// seq 字段（协议 2 起可选）见 api/signal/envelope.schema.json，由 #32 接线。
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

// Decode 将信封 data 反序列化到 v。缺 data 视为错误（服务端上行必须带载荷）。
func (e Envelope) Decode(v any) error {
	if len(e.Data) == 0 {
		return errors.New("信令缺少 data 字段")
	}
	return json.Unmarshal(e.Data, v)
}

// ---- 鉴权（服务端专属，不进 schema） ----

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
	// （dev 令牌无会话概念；生产构建拒绝启用 devAuth，见 cmd/server）。
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

// ---- ICE 与 pion 互操作 ----

// ICECandidateData rtc.ice 与 pion 的互操作别名。wire 形状由生成物 ICEData 承担
// （schema 里字段名沿用 WebRTC 标准），本别名只服务 sfu.AddICECandidate 的入参类型。
type ICECandidateData = webrtc.ICECandidateInit

// toICECandidate 生成物 ICEData → pion。可选字段的指针语义在这里对齐
// （pion 是 *string，ICEData 是 omitempty 的 string）。
func toICECandidate(d ICEData) ICECandidateData {
	out := ICECandidateData{Candidate: d.Candidate, SDPMLineIndex: d.SDPMLineIndex}
	if d.SDPMid != "" {
		mid := d.SDPMid
		out.SDPMid = &mid
	}
	if d.UsernameFragment != "" {
		ufrag := d.UsernameFragment
		out.UsernameFragment = &ufrag
	}
	return out
}

// fromICECandidate pion → 生成物 ICEData（下行 rtc.ice）。
func fromICECandidate(c ICECandidateData) ICEData {
	out := ICEData{Candidate: c.Candidate, SDPMLineIndex: c.SDPMLineIndex}
	if c.SDPMid != nil {
		out.SDPMid = *c.SDPMid
	}
	if c.UsernameFragment != nil {
		out.UsernameFragment = *c.UsernameFragment
	}
	return out
}
