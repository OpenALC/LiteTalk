package bootstrap

import (
	"context"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

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
