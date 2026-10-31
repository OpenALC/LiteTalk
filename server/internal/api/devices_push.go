package api

// devices_push.go 推送触发点的 api 侧胶水（#53）。三个触发点各自的调用位置：
//   - CreateMessage（messages.go，发送成功后）→ 本文件 notifyMessageCreate：
//     同一行调用同时覆盖 @提及与私聊（direct 作用域）两条触发链；
//   - CreateFriendRequest（friends.go，建关系成功后）→ notifyFriendRequest。
// Broker 内部异步派发（带缓冲 channel + worker）：离线判定、双频控与设备查询
// 全在 worker，入队只做一次 channel send，绝不阻塞消息写路径。
// 推送关闭（s.push == nil）时全部短路，零额外开销。

import (
	"context"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// notifyMessageCreate 频道消息发送成功后的推送触发（@提及 / 私聊）。
// body 文案由 push.Broker 生成——只描述事实（「在 #频道 提到你」），
// 绝不携带消息正文（锁屏泄露红线）。
func (s *Server) notifyMessageCreate(ctx context.Context, ch *store.Channel, msg *store.Message, senderNick string) {
	if s.push == nil {
		return
	}
	s.push.NotifyMessage(ctx, ch, msg, senderNick)
}

// notifyFriendRequest 好友请求发出成功后的推送触发：通知被请求方。
func (s *Server) notifyFriendRequest(ctx context.Context, targetUID, fromUID int64, fromNick string) {
	if s.push == nil {
		return
	}
	s.push.NotifyFriendRequest(ctx, targetUID, fromUID, fromNick)
}
