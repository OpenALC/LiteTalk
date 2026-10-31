package api

// oidc.go — OIDC 登录 REST（#54）。authorize 换 IdP 授权 URL；callback 用
// code+state 换与本地登录同构的 TokenPair（前端零改动）。未配置 LITETALK_OIDC_*
// 时两个端点一律 404（功能关闭零副作用：无 discovery、无状态表、无后台协程）。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 频控键前缀（批次 6 §0.4）：OIDC 归属用独立 `oidc:`（而非 `tok:`）——两个端点
// 都是免鉴权登录面、按 IP 限流，与令牌管理端点（tok:，按 uid、Bearer 在先）的
// 主体与语义都不同，混用前缀会让排查频控问题时无法按功能面定位。
const (
	oidcAuthorizeLimit = 20 // 每分钟每 IP：正常人多标签页也用不到 20 次发起
	oidcCallbackLimit  = 20 // 每分钟每 IP：state 5 分钟 TTL 内的有效回调屈指可数
	oidcCreateRetries  = 5  // 建号冲突重试（同注册的 tag 重摇口径）
	// oidcNicknameMaxRunes 与 users.nickname VARCHAR(16) 同口径。
	oidcNicknameMaxRunes = 16
)

// OidcAuthorize POST /api/v1/auth/oidc/authorize：生成 state（服务端存 5 分钟、
// 一次性消费）与 PKCE，返回 IdP 授权页 URL。code_verifier 明文不出服务端。
func (s *Server) OidcAuthorize(ctx context.Context, request OidcAuthorizeRequestObject) (OidcAuthorizeResponseObject, error) {
	if s.oidc == nil {
		return nil, errf(http.StatusNotFound, "not_found", "OIDC 登录未启用")
	}
	if e := s.allowRate(ctx, "oidc:auth:"+reqIP(ctx), oidcAuthorizeLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	dev := auth.DeviceDesktop
	if request.Body != nil {
		d, ok := deviceType(deref(request.Body.Device))
		if !ok {
			return nil, errf(http.StatusBadRequest, "bad_device", "设备类型不合法（仅 desktop/mobile）")
		}
		dev = d
	}
	// challenge 只进授权 URL（由 Provider 内部计算），handler 不需要单独持有。
	verifier, _, err := auth.NewOIDCPKCE()
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	nonce, err := randText(16)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	state, err := s.oidcStates.Issue(ctx, auth.OIDCState{
		Verifier: verifier,
		Nonce:    nonce,
		Dev:      dev,
		IP:       reqIP(ctx),
	})
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	url, err := s.oidc.AuthorizeURL(ctx, state, verifier, nonce)
	if err != nil {
		// discovery 失败：state 已落存但无回调可消费它，5 分钟 TTL 自净。
		return nil, s.oidcUpstreamErr(err)
	}
	return OidcAuthorize200JSONResponse{Url: url}, nil
}

// OidcCallback POST /api/v1/auth/oidc/callback：state 一次性消费 → 授权码换
// ID Token（PKCE + RS256 验签）→ 绑定或建号 → 签发与本地登录同构的令牌对。
func (s *Server) OidcCallback(ctx context.Context, request OidcCallbackRequestObject) (OidcCallbackResponseObject, error) {
	if s.oidc == nil {
		return nil, errf(http.StatusNotFound, "not_found", "OIDC 登录未启用")
	}
	if e := s.allowRate(ctx, "oidc:cb:"+reqIP(ctx), oidcCallbackLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	code := strings.TrimSpace(request.Body.Code)
	state := strings.TrimSpace(request.Body.State)
	if code == "" || state == "" {
		return nil, errf(http.StatusBadRequest, "bad_request", "缺少 code 或 state")
	}
	st, err := s.oidcStates.Consume(ctx, state)
	if err != nil {
		// 未知/过期/重放统一口径：让用户重新发起，不区分原因（不给重放试探反馈）。
		return nil, errf(http.StatusBadRequest, "bad_state", "登录状态无效或已过期，请重新发起登录")
	}
	claims, err := s.oidc.Exchange(ctx, code, st.Verifier, st.Nonce)
	if err != nil {
		return nil, s.oidcUpstreamErr(err)
	}
	user, err := s.oidcEnsureAccount(ctx, claims, s.oidc.Issuer())
	if err != nil {
		return nil, err
	}
	pair, err := s.issueTokens(ctx, user, st.Dev)
	if err != nil {
		return nil, err
	}
	return OidcCallback200JSONResponse(*pair), nil
}

// oidcUpstreamErr IdP 侧错误 → HTTP 语义。验证类失败统一让用户重新发起；
// upstream 记录底层错误（500 规范同源：对外折叠、对内留日志）。
func (s *Server) oidcUpstreamErr(err error) *apiErr {
	switch {
	case errors.Is(err, auth.ErrOIDCCode):
		return errf(http.StatusBadRequest, "bad_code", "授权码无效或已过期，请重新发起登录")
	case errors.Is(err, auth.ErrOIDCToken):
		return errf(http.StatusBadRequest, "bad_code", "身份令牌校验失败，请重新发起登录")
	case errors.Is(err, auth.ErrOIDCUpstream):
		s.log.Printf("[oidc] IdP 故障: %v", err)
		return errf(http.StatusBadRequest, "oidc_upstream", "身份提供方暂时不可用，请稍后再试")
	}
	s.log.Printf("[oidc] 未知错误: %v", err)
	return errf(http.StatusInternalServerError, "internal", "服务器内部错误")
}

// oidcEnsureAccount 按 (issuer, sub) 解析出可登录账号：已有绑定直接登录；
// 首次登录时 email_verified 且邮箱命中既有用户才自动绑定（防账号劫持），
// 其余一律新建账号（绝不自动合并）。并发撞邮箱/绑定键时限轮重查收敛。
func (s *Server) oidcEnsureAccount(ctx context.Context, claims *auth.OIDCClaims, issuer string) (*store.User, error) {
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	for i := 0; i < oidcCreateRetries; i++ {
		// 1) 已有绑定：直接登录。属主被删/停用 → 拒绝（与令牌路径同一 status 口径）。
		idn, err := s.store.OIDCIdentityByIssuerSub(ctx, issuer, claims.Sub)
		switch {
		case err == nil:
			user, uerr := s.store.UserByID(ctx, idn.UserID)
			if uerr != nil || user.Status != 0 {
				return nil, errf(http.StatusForbidden, "account_disabled", "账号已被停用")
			}
			return user, nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, s.writeStoreErr(ctx, err)
		}
		// 2) 首次登录：email_verified 且邮箱命中既有用户 → 自动绑定。
		if email != "" && claims.EmailVerified {
			existing, eerr := s.store.UserByEmail(ctx, email)
			switch {
			case eerr == nil && existing.Status != 0:
				// 命中的本地账号已停用：既不绑定也不另建号（防身份遮蔽），明确拒绝。
				return nil, errf(http.StatusForbidden, "account_disabled", "账号已被停用")
			case eerr == nil:
				if cerr := s.store.CreateOIDCIdentity(ctx, &store.OIDCIdentity{
					Issuer: issuer, Sub: claims.Sub, UserID: existing.ID, Email: email,
				}); cerr != nil {
					if !errors.Is(cerr, store.ErrConflict) {
						return nil, s.writeStoreErr(ctx, cerr)
					}
					// (issuer, sub) 已被并发首登绑定：重查一轮走绑定分支。
					continue
				}
				s.log.Printf("[audit] oidc 绑定既有账号 issuer=%s sub=%s user=%d ip=%s",
					issuer, claims.Sub, existing.ID, reqIP(ctx))
				return existing, nil
			case !errors.Is(eerr, store.ErrNotFound):
				return nil, s.writeStoreErr(ctx, eerr)
			}
			// 邮箱未注册：以该已验证邮箱建号（并发被注册时 ErrConflict 落到轮次重查）。
			user, cerr := s.oidcCreateUser(ctx, email, true, claims)
			if cerr != nil && !errors.Is(cerr, store.ErrConflict) {
				return nil, s.writeStoreErr(ctx, cerr)
			}
			if cerr == nil {
				return s.oidcBindNew(ctx, user, issuer, claims)
			}
			continue
		}
		// 3) 无邮箱或未验证：占位邮箱建号。绝不把未经证实的 email 写上账号——
		// 恶意声明可借唯一索引占住该邮箱，真实主人之后就无法注册它了。
		placeholder := "oidc-" + randHex(8) + "@oidc.local"
		user, cerr := s.oidcCreateUser(ctx, placeholder, false, claims)
		if cerr != nil && !errors.Is(cerr, store.ErrConflict) {
			return nil, s.writeStoreErr(ctx, cerr)
		}
		if cerr == nil {
			return s.oidcBindNew(ctx, user, issuer, claims)
		}
		// 占位邮箱撞车（8 随机字节几乎不可能）：重摇重试。
	}
	return nil, errf(http.StatusInternalServerError, "internal", "创建账号失败")
}

// oidcBindNew 建号后落绑定行并回读登录态。绑定行 (issuer, sub) 冲突说明并发
// 首登已经绑定到另一账号：重查绑定返回既有账号（幂等收敛；本轮可能留下一个
// 未绑定的孤儿用户行，无法登录，仅占号不构成安全问题）。
func (s *Server) oidcBindNew(ctx context.Context, user *store.User, issuer string, claims *auth.OIDCClaims) (*store.User, error) {
	if err := s.store.CreateOIDCIdentity(ctx, &store.OIDCIdentity{
		Issuer: issuer, Sub: claims.Sub, UserID: user.ID, Email: user.Email,
	}); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return nil, s.writeStoreErr(ctx, err)
		}
		idn, lerr := s.store.OIDCIdentityByIssuerSub(ctx, issuer, claims.Sub)
		if lerr != nil {
			return nil, s.writeStoreErr(ctx, lerr)
		}
		return s.store.UserByID(ctx, idn.UserID)
	}
	s.log.Printf("[audit] oidc 建号 issuer=%s sub=%s user=%d email=%s ip=%s",
		issuer, claims.Sub, user.ID, user.Email, reqIP(ctx))
	return user, nil
}

// oidcCreateUser 新建 OIDC 账号：无本地密码（空哈希在 CheckPassword 恒失败，
// 与 bot/游客同口径 fail-closed）；昵称取 IdP 声明，tag 随机，(nickname, tag)
// 撞唯一键时由调用方限轮重试（复用注册路径的随机 tag 口径，不自造哈希后缀）。
func (s *Server) oidcCreateUser(ctx context.Context, email string, emailVerified bool, claims *auth.OIDCClaims) (*store.User, error) {
	user := &store.User{
		Email:         email,
		Nickname:      oidcNickname(claims),
		Tag:           randDigits(4),
		PasswordHash:  "",
		EmailVerified: emailVerified,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// oidcNickname 从 IdP 声明挑昵称（preferred_username > name > email 本地段），
// 截到 16 rune、不足 2 rune 回退随机占位（与 validNickname 的 2–16 区间同口径）。
// 与既有用户重名不冲突：唯一键是 (nickname, tag)，tag 随机撞车由重试消化。
func oidcNickname(claims *auth.OIDCClaims) string {
	base := strings.TrimSpace(claims.PreferredUsername)
	if base == "" {
		base = strings.TrimSpace(claims.Name)
	}
	if base == "" {
		if i := strings.IndexByte(claims.Email, '@'); i > 0 {
			base = claims.Email[:i]
		}
	}
	runes := []rune(base)
	if len(runes) > oidcNicknameMaxRunes {
		runes = runes[:oidcNicknameMaxRunes]
		base = string(runes)
	}
	if len([]rune(strings.TrimSpace(base))) < 2 {
		return "用户" + randDigits(4)
	}
	return base
}

// randText n 字节随机量的 base64url 串（nonce 用）。
func randText(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
