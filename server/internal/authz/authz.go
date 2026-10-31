// Package authz 是 #35 B2 的权限判断核心：与 UI/API 无关，只吃 store.Store。
// 语义来源是 B1 的角色/分配/频道覆盖三张表；角色数据缺失时回退 legacy 静态模板。
// handler 约定：(false, nil) 统一 403，error 转 500——故障绝不能伪装成无权限。
package authz

import (
	"context"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// Can 判断 uid 对频道的权限。channelID 必须是真实频道（不存在返回 store.ErrNotFound）；
// 服务器级判断用 CanServer/CanInServer(serverID, 0)，本方法传 0 只会得到 ErrNotFound。
func (a *Authorizer) Can(ctx context.Context, uid, channelID int64, perm store.Permission) (bool, error) {
	ch, err := a.st.ChannelByID(ctx, channelID)
	if err != nil {
		return false, err
	}
	return a.CanInServer(ctx, uid, ch.ServerID, channelID, perm)
}

// CanInServer 调用方已持有 server 上下文时用它省一次频道查询。
// channelID == 0 视为服务器级（不看频道覆盖）；perm 含频道位时同样按服务器级
// 处理，不报错。channelID 属于 serverID 由调用方保证，这里不回查（错配时该频道
// 在本服快照里没有覆盖，退化为服务器级结果）。
//
// #54 scope 上限的唯一交集点：ctx 里带 api_token 的 Principal（pat/bot）时，
// 最终权限 = 角色权限 ∩ scope 映射位。检查的权限位超出 scope 上限直接拒绝、
// 不查角色——scope 绝不授予只收紧，角色降级后令牌自动失去对应权限。
// ctx 无 Principal（WS 旧路径）或 JWT 主体时不受限，行为与之前完全一致。
func (a *Authorizer) CanInServer(ctx context.Context, uid, serverID, channelID int64, perm store.Permission) (bool, error) {
	if ok, err := precheck(perm); err != nil || ok {
		return ok, err
	}
	if limit, ok := token.ScopeLimitFromContext(ctx); ok && perm&^limit != 0 {
		return false, nil
	}
	snap, err := a.policyFor(ctx, serverID)
	if err != nil {
		return false, err
	}
	return snap.effective(uid, channelID, perm), nil
}

// CanServer 只做服务器级判断，不看频道覆盖。
func (a *Authorizer) CanServer(ctx context.Context, uid, serverID int64, perm store.Permission) (bool, error) {
	return a.CanInServer(ctx, uid, serverID, 0, perm)
}

// IsMember 成员判断（纯成员表；owner 不旁路——归属判断用 Can*）。
func (a *Authorizer) IsMember(ctx context.Context, uid, serverID int64) (bool, error) {
	snap, err := a.policyFor(ctx, serverID)
	if err != nil {
		return false, err
	}
	return snap.isMember(uid), nil
}

// precheck 前置短路：perm == 0 无要求直接放行；未定义位拒绝（按 403 处理）。
// 两条都在查缓存之前——垃圾入参不值得打 Store。
func precheck(perm store.Permission) (bool, error) {
	if perm == 0 {
		return true, nil
	}
	if !perm.Valid() {
		return false, store.ErrForbidden
	}
	return false, nil
}
