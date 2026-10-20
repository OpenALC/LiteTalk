package bootstrap

import (
	"context"
	"errors"
	"sync"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

var (
	// claimMu 串行化「复查未认领 + 建默认服务器」（#27 P2）：并发带码注册若都通过
	// 前置 check，会各建一个默认服务器。只覆盖单实例；多实例 MySQL 部署还需
	// 数据库事务/唯一约束，本任务不覆盖。
	claimMu sync.Mutex
	// ErrClaimed 实例已被认领（含并发请求抢先认领）；调用方只记日志，不回滚注册。
	ErrClaimed = errors.New("bootstrap: 实例已被认领")
)

// Claim 原子认领：锁内按持久状态复查，再建默认服务器并写消费标记。
// 消费标记写失败不返回错误——服务器已落库，Required 的 HasServers 会持续拒绝复用。
func Claim(ctx context.Context, kv store.KV, st store.Store, ownerID int64) (*store.Server, error) {
	claimMu.Lock()
	defer claimMu.Unlock()
	if !Required(ctx, kv, st) {
		return nil, ErrClaimed
	}
	srv, err := Adopt(ctx, st, ownerID)
	if err != nil {
		return nil, err
	}
	if kv != nil {
		_ = Consume(ctx, kv, ownerID)
	}
	return srv, nil
}

// Adopt 注册带码成功后以该用户为 owner 创建默认服务器（§0.8）。
// 建服务器 + owner 成员 + 文字/语音频道各一（「可省」但空服务器开箱不可用）。
// 失败返回错误：调用方按 500 处理——用户已建好，服务器可事后手动补建。
func Adopt(ctx context.Context, st store.Store, ownerID int64) (*store.Server, error) {
	srv := &store.Server{
		Name:      DefaultServerName,
		OwnerID:   ownerID,
		IconName:  "messages-square",
		IconColor: "#4FA4F3",
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		return nil, err
	}
	mem := &store.ServerMember{ServerID: srv.ID, UserID: ownerID, Role: store.RoleOwner}
	if err := st.AddMember(ctx, mem); err != nil {
		return nil, err
	}
	if err := st.CreateChannel(ctx, &store.Channel{
		ServerID: srv.ID, Name: "文字频道", Type: store.ChannelText,
	}); err != nil {
		return nil, err
	}
	if err := st.CreateChannel(ctx, &store.Channel{
		ServerID: srv.ID, Name: "语音频道", Type: store.ChannelVoice,
	}); err != nil {
		return nil, err
	}
	return srv, nil
}
