package api

import (
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// PublicUser 对外用户信息视图：不含 email 等隐私字段（P1）。
// 昵称#tag 只有 1 万种组合可枚举，好友/成员等「返回他人信息」的响应必须用它；
// 只有本人 /me 与登录/注册等自响应可以带 email。
type PublicUser struct {
	ID            int64     `json:"id"`
	Nickname      string    `json:"nickname"`
	Tag           string    `json:"tag"`
	EmailVerified bool      `json:"email_verified"`
	IsGuest       bool      `json:"is_guest"`
	Status        int8      `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// publicUser 从完整用户记录投影出公开视图；nil 安全（好友对端查询失败时透传 nil）。
func publicUser(u *store.User) *PublicUser {
	if u == nil {
		return nil
	}
	return &PublicUser{
		ID:            u.ID,
		Nickname:      u.Nickname,
		Tag:           u.Tag,
		EmailVerified: u.EmailVerified,
		IsGuest:       u.IsGuest,
		Status:        u.Status,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
	}
}
