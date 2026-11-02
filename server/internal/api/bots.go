// bots.go bot 账号在 REST 侧的过滤与准入（#51）。判定数据只有一处口径：
// users.is_bot，集合查询走 store.BotsAmong——各 handler 不得自行拼 is_bot 条件。
// bot 账号本身由管理员 CLI（cmd/litetalkctl）创建，REST 不开创建入口。
package api

import (
	"context"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// filterBots 列表过滤的唯一实现（#51）：剔除 uid(item) 命中 bot 的元素，保序。
// 成员管理列表（人对人的管理面）与 @提及候选共用；好友候选不走列表过滤——
// bot 进不了好友关系（好友请求入口拒绝，见 rejectBotTarget）。
// 查询故障上抛 500：把故障静默当成「没有 bot」会无差别放行。
func filterBots[T any](ctx context.Context, st store.Store, list []T, uid func(T) int64) ([]T, error) {
	ids := make([]int64, 0, len(list))
	for _, item := range list {
		ids = append(ids, uid(item))
	}
	bots, err := st.BotsAmong(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(list))
	for _, item := range list {
		if !bots[uid(item)] {
			out = append(out, item)
		}
	}
	return out, nil
}

// rejectBotTarget 好友请求的 bot 准入门（#51）：bot 不进好友候选——按 昵称#tag
// 找到的是 bot 时与「用户不存在」同形（404 not_found），不泄露该名字是否被
// bot 占用。
func rejectBotTarget(target *store.User) *apiErr {
	if target.IsBot {
		return errf(http.StatusNotFound, "not_found", "用户不存在")
	}
	return nil
}
