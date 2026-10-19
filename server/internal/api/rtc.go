package api

import (
	"context"
	"net/http"
	"strings"
)

// GetHealthz GET /api/v1/healthz（免鉴权，#77 收编）。
// 部署脚本/探针依赖该路径与方法，响应形状固定为 {status:"ok", version}。
func (s *Server) GetHealthz(ctx context.Context, request GetHealthzRequestObject) (GetHealthzResponseObject, error) {
	return GetHealthz200JSONResponse{
		Status:  HealthzStatusOk,
		Version: s.version,
	}, nil
}

// PostRtcCredentials POST /api/v1/rtc/credentials（#77 收编）。
// 鉴权方式与信令一致：Authorization: Bearer 或 ?token=，由 main 注入的
// RTCAuth 解析 uid（保留 dev 令牌兜底）。未配置 TURN 密钥时返回 503，
// 客户端据此降级为直连，不阻断进房。
func (s *Server) PostRtcCredentials(ctx context.Context, request PostRtcCredentialsRequestObject) (PostRtcCredentialsResponseObject, error) {
	if s.rtcAuth == nil {
		// fail-closed：未注入鉴权函数时不放行，文案不暴露内部原因。
		return nil, errf(http.StatusUnauthorized, "unauthorized", "缺少访问令牌")
	}
	token := tokenFromRequest(httpRequest(ctx))
	if token == "" {
		return nil, errf(http.StatusUnauthorized, "unauthorized", "缺少访问令牌")
	}
	uid, err := s.rtcAuth(token)
	if err != nil || uid == "" {
		// 不把底层解析错误（过期/签名/会话细节）写进响应。
		return nil, errf(http.StatusUnauthorized, "unauthorized", "访问令牌无效或已过期")
	}
	if s.rtcCfg.Secret == "" {
		return nil, errf(http.StatusServiceUnavailable, "turn_unavailable", "TURN 未配置")
	}

	username, credential, expiry := s.rtcCfg.Issue(uid)
	resp := PostRtcCredentials200JSONResponse{
		IceServers: []ICEServer{},
		ExpiresAt:  expiry.Unix(),
	}
	// STUN 条目不带凭证；TURN 条目带短期用户名/凭证（coturn REST API 口径）。
	if len(s.rtcCfg.StunURLs) > 0 {
		resp.IceServers = append(resp.IceServers, ICEServer{Urls: s.rtcCfg.StunURLs})
	}
	if len(s.rtcCfg.TurnURLs) > 0 {
		user, cred := username, credential
		resp.IceServers = append(resp.IceServers, ICEServer{
			Urls:       s.rtcCfg.TurnURLs,
			Username:   &user,
			Credential: &cred,
		})
	}
	return resp, nil
}

// httpRequest strict 方法拿不到 *http.Request，由 withStrictCtx 注入 ctx。
func httpRequest(ctx context.Context) *http.Request {
	r, _ := ctx.Value(httpReqKey).(*http.Request)
	return r
}

// tokenFromRequest 提取访问令牌：优先 Authorization: Bearer，其次 ?token=。
// ?token= 是 WS 信令与旧客户端的兼容形态，rtc/credentials 保持同一口径。
func tokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}
