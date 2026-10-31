// bots.go bot 账号在信令侧的过滤（#51）：presence 广播不含 bot。好友/@提及候选
// 的过滤在 REST 层（api.Server.filterBotMembers）；这里只守事件面。
package signal

import (
	"context"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// botUserLookup 按 uid 回读用户行（#51）。store.Store 满足；ChatStore 未包含
// 该方法，测试桩可不实现（按非 bot 处理，维持原行为）。
type botUserLookup interface {
	UserByID(ctx context.Context, id int64) (*store.User, error)
}

// uidIsBot uid 是否为 bot 账号。chatStore 无 UserByID、或查询故障时按非 bot——
// 这是脏数据兜底（bot 正常没有好友关系，好友请求入口在 REST 层已封），不值得
// 为它引入故障分支拖慢 presence 边沿。
func (h *Hub) uidIsBot(uid string) bool {
	look, ok := h.chatStore.(botUserLookup)
	if !ok {
		return false
	}
	id, err := parseUID(uid)
	if err != nil {
		return false // dev 令牌等非数字 uid 无 bot 概念
	}
	u, err := look.UserByID(context.Background(), id)
	if err != nil {
		return false
	}
	return u.IsBot
}
