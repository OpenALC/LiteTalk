package api

import (
	"context"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// PublicUser 由 openapi.yaml 生成（api.gen.go）：对外用户视图，不含 email。
// 昵称#tag 只有 1 万种组合可枚举，好友/成员等「返回他人信息」的响应必须用它；
// 只有本人 /me 与登录/注册等自响应可以带 email（User）。

// publicUser 从完整用户记录投影出公开视图；nil 安全（好友对端查询失败时透传 nil）。
func publicUser(u *store.User) *PublicUser {
	if u == nil {
		return nil
	}
	return &PublicUser{
		Id:            u.ID,
		Nickname:      u.Nickname,
		Tag:           u.Tag,
		EmailVerified: u.EmailVerified,
		IsGuest:       u.IsGuest,
		Status:        int32(u.Status),
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
	}
}

// userView 组装对外用户视图并填在线态（#11）。好友数量小，逐个查 presence:{uid} 即可；
// 与 signal.presenceKey 的键格式手工同步（两包互不依赖）。
func (s *Server) userView(ctx context.Context, u *store.User) *PublicUser {
	pub := publicUser(u)
	if pub == nil {
		return nil
	}
	pub.Online = s.online(ctx, u.ID)
	return pub
}

// online presence:{uid} 存在 = 在线；无 KV 依赖时恒离线。
func (s *Server) online(ctx context.Context, uid int64) bool {
	if s.presence == nil {
		return false
	}
	_, err := s.presence.Get(ctx, "presence:"+strconv.FormatInt(uid, 10))
	return err == nil
}
