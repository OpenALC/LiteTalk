// Package signal 实现轻音 LiteTalk 客户端的 WebSocket 信令层。
//
// 协议契约的唯一事实来源是 api/signal/（JSON Schema）：消息类型常量、载荷结构、
// 错误码、close code、协议版本都由 server/api/tools/signalgen 生成进 protocol_gen.go，
// 两侧生成物字节相同。改契约请改 api/signal/ 并重新生成。
//
// 本文件只保留**手写运行时层**（生成器不管的职责）：
//   - 信封 Envelope / NewEnvelope / Decode（与服务端 MarshalEnvelope 语义略有差异）
//   - Bool（可选增量字段的指针构造，客户端专属）
//
// 禁止把上述内容塞进 schema；禁止手改 protocol_gen.go。
package signal

import (
	"encoding/json"
	"time"
)

// nowMillis 当前 Unix 毫秒（信封 ts）。
func nowMillis() int64 { return time.Now().UnixMilli() }

// Envelope 信令信封。Data 延迟解析，由订阅者按 Type 自行解码。
// seq 字段（协议 2 起可选）见 api/signal/envelope.schema.json，由 #32 接线。
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

// Decode 解析 Data 到 v（Data 为空时返回 nil 错误，v 保持零值——
// 服务端侧 Decode 对缺 data 报错，两侧语义按各自调用场景保留）。
func (e Envelope) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// Bool 返回 b 的指针（构造可选增量字段用）。
func Bool(b bool) *bool { return &b }
