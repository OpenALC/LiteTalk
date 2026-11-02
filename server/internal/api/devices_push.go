package api

// devices_push.go 推送触发点的 api 侧胶水（#53）。
//   - 频道消息（@提及/私聊）：不再由 REST 单独触发（P1-4 此前只在 REST 接线，
//     走 WS 的官方客户端永不触发）——消息创建后处理已收敛为
//     signal.Hub.AfterMessagePersisted 单一管线（P2-1），WS chat.send、REST
//     CreateMessage、入站 webhook 投递三条入口经 afterMessageCreate 汇入同一段
//     实现；推送是管线的最后一步，由 hub 持有的 PushNotifier（*push.Broker，
//     main.go 启动阶段 SetPushNotifier 注入）异步派发。
//   - CreateFriendRequest（friends.go，建关系成功后）→ notifyFriendRequest：
//     与消息管线无关，仍在 REST 成功点直接触发。
// Broker 内部异步派发（带缓冲 channel + worker）：离线判定、双频控与设备查询
// 全在 worker，入队只做一次 channel send，绝不阻塞消息写路径。
// 推送未注入时管线内短路，零额外开销。

import (
	"context"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// afterMessageCreate 消息创建后管线的 api 侧统一入口（P1-4/P2-1）：REST 与入站
// webhook 落库成功后调用，与 WS handleChatSend 汇入同一个
// signal.Hub.AfterMessagePersisted。hub 为 nil（无信令面的测试/降级形态）时管线
// 整体不可用——广播/未读/推送/出站 webhook 无从谈起，直接短路。
func (s *Server) afterMessageCreate(ctx context.Context, ch *store.Channel, msg *store.Message, senderUID int64, senderNick string) {
	if s.hub == nil {
		return
	}
	s.hub.AfterMessagePersisted(ctx, ch, msg, strconv.FormatInt(senderUID, 10), senderNick)
}

// notifyFriendRequest 好友请求发出成功后的推送触发：通知被请求方。
func (s *Server) notifyFriendRequest(ctx context.Context, targetUID, fromUID int64, fromNick string) {
	if s.push == nil {
		return
	}
	s.push.NotifyFriendRequest(ctx, targetUID, fromUID, fromNick)
}
