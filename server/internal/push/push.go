// Package push 移动推送抽象（#53），与 mailer 同构的可插拔接口：
//   - Ntfy：HTTP POST JSON（开源友好，唯一能在 CI 里真实测的通道）
//   - FCM：Firebase HTTP v1 + 服务账号（stdlib RSA 签名，零新依赖）
//   - APNs：token-based JWT（stdlib ECDSA，零新依赖）
//   - Noop：默认实现，不做任何 I/O——"未配置 = 功能关闭且零副作用"是硬要求
//
// 触发条件由 Broker 统一把关：该用户所有设备都不在线才推（连接表实时判定），
// 且同一用户 10 分钟内最多 1 条；派发走带缓冲 channel + worker，绝不阻塞消息写路径。
//
// 隐私红线（写死在实现里）：Notification 只承载 {title, body, route}，
// body 用「在 #频道 提到你」这类事实描述，**绝不放消息正文**——锁屏预览会被
// 旁人看到。所有实现对 push_token 零日志：token 是生产凭据的等价物，
// 万一要打日志只留前 8 位（MaskToken）。
package push

import (
	"context"
	"log"
)

// Device 一台已注册的推送设备（store.PushDevice 的投递视图，Platform+Token 即可触达）。
type Device struct {
	Platform string // ios / android
	Token    string // 推送通道的设备凭据（FCM token / APNs device token / ntfy topic）
}

// Notification 极简推送内容。Route 是客户端点击后的路由（如 "channel:123"、
// "friends"），由移动端映射到页面；服务端不解释其格式。
type Notification struct {
	Title string // 通常是触发者昵称或应用名
	Body  string // 事实描述，不含消息正文（锁屏泄露红线，见包注释）
	Route string // 客户端路由提示
}

// Pusher 推送投递接口（mailer 同构）。实现不得打印 Device.Token。
type Pusher interface {
	// Push 向一批设备投递同一条通知。部分设备失败不中断其余设备——
	// 返回 error 仅表示「需要上层记录」（多设备失败实现方自行聚合）。
	Push(ctx context.Context, devices []Device, n Notification) error
}

// Noop 默认实现：不做任何 I/O，直接成功。push 未配置时 New 返回它，
// 保证关闭状态下零外部副作用（不建连接、不解析凭据、无 goroutine）。
type Noop struct{}

// Push 空实现。
func (Noop) Push(context.Context, []Device, Notification) error { return nil }

// MaskToken 日志打码：只留前 8 位，其余以 … 代替。短 token 也照常截断，
// 避免分支判断反而泄露长度信息。
func MaskToken(token string) string {
	if len(token) <= 8 {
		return token + "…"
	}
	return token[:8] + "…"
}

// safeLog 所有实现共用：注入 nil logger 时不崩。
func safeLog(lg *log.Logger) *log.Logger {
	if lg == nil {
		return log.Default()
	}
	return lg
}
