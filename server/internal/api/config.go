package api

import (
	"context"
)

// GetConfig GET /api/v1/config（免鉴权）：
// 返回服务端能力开关，客户端据此适配注册/找回密码 UI。
// mail_configured 表示发信通道是否为真实投递（非 Console 打印）。
// self_host 一键自托管形态（SQLite/进程内单机部署）；bootstrap_required
// 管理员邀请码是否尚未被消费（注册页提示「等待管理员邀请」，#26/#27）。
// push_enabled 移动推送是否已配置可用通道（#53；false = 仅不派发，
// /devices 注册端点仍可用）。
func (s *Server) GetConfig(ctx context.Context, request GetConfigRequestObject) (GetConfigResponseObject, error) {
	bootstrapRequired := false
	if s.bootstrapRequired != nil {
		bootstrapRequired = s.bootstrapRequired()
	}
	// push_enabled 在 schema 里可缺省（新增响应字段的兼容口径），实际恒返回。
	pushEnabled := s.pushEnabled()
	return GetConfig200JSONResponse{
		AuthMode:          ServerConfigAuthMode(s.authMode),
		MailConfigured:    s.mailConfigured(),
		SelfHost:          s.selfHost,
		BootstrapRequired: bootstrapRequired,
		PushEnabled:       &pushEnabled,
	}, nil
}
