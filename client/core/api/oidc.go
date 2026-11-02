package api

// oidc.go — OIDC 登录客户端（#118 P2-5）。authorize 换 IdP 授权页 URL；
// code+state 的令牌交换只发生在前端回调页（web 构建直连 REST），桌面端不参与
// 交换——IdP 重定向到部署配置的 redirect_url，会话落在服务端托管的 Web 客户端。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OidcAuthorizeResponse /auth/oidc/authorize 响应（契约 OidcAuthorizeResponse）。
type OidcAuthorizeResponse struct {
	// URL IdP 授权页 URL（含 state 与 PKCE challenge，直接重定向浏览器）。
	URL string `json:"url"`
}

// OidcAuthorize 发起 OIDC 登录：服务端生成一次性 state 与 PKCE（verifier 不出
// 服务端），返回 IdP 授权页 URL。调用方把系统浏览器带到该 URL 即可；认证完成
// 后 IdP 重定向回部署配置的 redirect_url（前端回调页），由回调页把 code+state
// POST 回 /auth/oidc/callback 换取令牌对。
func (c *APIClient) OidcAuthorize(ctx context.Context) (string, error) {
	status, data, err := c.postCtx(ctx, "/auth/oidc/authorize", nil, false)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", decodeError(status, data)
	}
	var r OidcAuthorizeResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("decode oidc authorize: %w", err)
	}
	if strings.TrimSpace(r.URL) == "" {
		return "", fmt.Errorf("decode oidc authorize: 响应缺少授权 URL")
	}
	return r.URL, nil
}
