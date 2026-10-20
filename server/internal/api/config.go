package api

import (
	"context"
)

// GetConfig GET /api/v1/config（免鉴权）：
// 返回服务端能力开关，客户端据此适配注册/找回密码 UI。
// mail_configured 表示发信通道是否为真实投递（非 Console 打印）。
// self_host 一键自托管形态（SQLite/进程内单机部署）；bootstrap_required
// 管理员邀请码是否尚未被消费（注册页提示「等待管理员邀请」，#26/#27）。
func (s *Server) GetConfig(ctx context.Context, request GetConfigRequestObject) (GetConfigResponseObject, error) {
	bootstrapRequired := false
	if s.bootstrapRequired != nil {
		bootstrapRequired = s.bootstrapRequired()
	}
	return GetConfig200JSONResponse{
		AuthMode:          ServerConfigAuthMode(s.authMode),
		MailConfigured:    s.mailConfigured(),
		SelfHost:          s.selfHost,
		BootstrapRequired: bootstrapRequired,
	}, nil
}
