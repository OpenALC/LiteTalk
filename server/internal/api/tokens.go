package api

// tokens.go 个人访问令牌 REST（#54）。登录是密码路径（auth.go），令牌是另一条
// 凭据体系：签发/列表/吊销在这里，校验在 auth 中间件的 ltk_ 分支 + internal/token。
// 明文只在 CreateToken 响应出现一次；列表/详情永不回传明文与 secret_hash。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// 频控（键前缀 tok:，批次 6 §0.4）：签发是低频管理动作按小时计，吊销按分钟计。
const (
	tokenCreateLimit = 10
	tokenRevokeLimit = 30
)

// requireJWTPrincipal 令牌管理面的主体门（P1-1）：签发/列举只认 JWT 主体。
// 令牌（pat/bot）的 scope 是上限而不是授予，任何非空集合都拦不住「先换
// guild.manage 再行事」的自签提权——签出的新令牌还不随原令牌吊销而失效，
// 等于给每个受限令牌留了持久化后门。PAT/bot 一律 403；JWT 行为不变。
func requireJWTPrincipal(ctx context.Context) *apiErr {
	p := token.PrincipalFromContext(ctx)
	if p == nil || p.Kind == token.KindJWT {
		return nil
	}
	return errf(http.StatusForbidden, "token_management_forbidden",
		"令牌管理仅限登录会话（JWT），访问令牌不可签发或列举其它令牌")
}

// ListTokens GET /api/v1/tokens（只回元数据，无 token 字段；仅限 JWT 主体，P1-1）。
func (s *Server) ListTokens(ctx context.Context, request ListTokensRequestObject) (ListTokensResponseObject, error) {
	if e := requireJWTPrincipal(ctx); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	rows, err := s.store.APITokensOfUser(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]ApiToken, 0, len(rows))
	for i := range rows {
		out = append(out, apiTokenWire(&rows[i]))
	}
	return ListTokens200JSONResponse(out), nil
}

// CreateToken POST /api/v1/tokens：仅限 JWT 主体（P1-1，先于频控与 body 校验）。
// 校验 scope 白名单 → 生成明文 → 落库 → 响应带一次性明文。空 scopes 明确 400
// （空 scope = 无权限，不是全权限）；未知/重复 scope 同样 400。
func (s *Server) CreateToken(ctx context.Context, request CreateTokenRequestObject) (CreateTokenResponseObject, error) {
	if e := requireJWTPrincipal(ctx); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.allowRate(ctx, "tok:create:"+strconv.FormatInt(c.UserID, 10),
		tokenCreateLimit, time.Hour, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	req := request.Body
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, errf(http.StatusBadRequest, "bad_name", "令牌名称需为 1–64 个字符")
	}
	scopes := make([]string, 0, len(req.Scopes))
	for _, sc := range req.Scopes {
		scopes = append(scopes, string(sc))
	}
	if !token.ValidScopes(scopes) {
		return nil, errf(http.StatusBadRequest,
			"bad_scopes", "scopes 必须非空、不重复且全部在白名单内")
	}
	var expires *time.Time
	if req.ExpiresAt != nil {
		t := time.Time(*req.ExpiresAt)
		if !t.After(time.Now()) {
			return nil, errf(http.StatusBadRequest, "bad_expires", "expires_at 必须晚于当前时刻")
		}
		expires = &t
	}

	var row *store.APIToken
	var plain *token.Plain
	var err error
	for i := 0; i < 3; i++ {
		plain, err = token.NewPlain(token.KindPAT)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		row = &store.APIToken{
			UserID:     c.UserID,
			Name:       name,
			Prefix:     plain.Prefix,
			SecretHash: plain.Hash,
			// bot 令牌的创建入口属 #51（管理员 CLI），REST 只签发 pat。
			Kind:      token.KindPAT,
			Scopes:    strings.Join(scopes, " "),
			ExpiresAt: expires,
		}
		err = s.store.CreateAPIToken(ctx, row)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, s.writeStoreErr(ctx, err)
		}
		// prefix 撞 uk_api_tokens_prefix：2^-32 量级，重摇重试即可。
	}
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.log.Printf("[audit] token created user=%d id=%d kind=%s scopes=%q ip=%s",
		c.UserID, row.ID, row.Kind, row.Scopes, reqIP(ctx))
	return CreateToken201JSONResponse(createdTokenWire(row, plain.Token)), nil
}

// RevokeToken DELETE /api/v1/tokens/{id}：吊销立即生效（校验路径不缓存 Principal）。
// 主体口径（P1-1）：任何主体都可吊销**自己握着的那张**令牌（pat/bot 按 TokenID
// 匹配——凭据泄露后的自毁通道）；管理其它令牌（含本人的其它令牌）仅限 JWT。
// 跨用户访问他人令牌与不存在统一 404（不可区分，避免枚举）；重复吊销幂等 200。
func (s *Server) RevokeToken(ctx context.Context, request RevokeTokenRequestObject) (RevokeTokenResponseObject, error) {
	c := currentUser(ctx)
	if e := s.allowRate(ctx, "tok:revoke:"+strconv.FormatInt(c.UserID, 10),
		tokenRevokeLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	if e := checkID(int64(request.Id)); e != nil {
		return nil, e
	}
	row, err := s.store.APITokenByID(ctx, int64(request.Id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(http.StatusNotFound, "not_found", "令牌不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if row.UserID != c.UserID {
		return nil, errf(http.StatusNotFound, "not_found", "令牌不存在")
	}
	// 令牌主体只能自毁（TokenID 匹配）；签发/列举的门在 requireJWTPrincipal。
	if p := token.PrincipalFromContext(ctx); p != nil && p.Kind != token.KindJWT && p.TokenID != row.ID {
		return nil, errf(http.StatusForbidden, "token_management_forbidden",
			"访问令牌只能吊销自身（泄露自毁），管理其它令牌需登录会话（JWT）")
	}
	if row.RevokedAt == nil {
		if err := s.store.RevokeAPIToken(ctx, row.ID, time.Now()); err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	s.log.Printf("[audit] token revoked user=%d id=%d ip=%s", c.UserID, row.ID, reqIP(ctx))
	return RevokeToken200JSONResponse{Status: "ok"}, nil
}

// apiTokenWire store 行 → 列表元数据（无明文、无哈希）。
func apiTokenWire(t *store.APIToken) ApiToken {
	out := ApiToken{
		Id:        idOf(t.ID),
		Name:      t.Name,
		Prefix:    t.Prefix,
		Kind:      ApiTokenKind(t.Kind),
		Scopes:    strings.Fields(t.Scopes),
		CreatedAt: timeOf(t.CreatedAt),
	}
	if t.ExpiresAt != nil {
		v := timeOf(*t.ExpiresAt)
		out.ExpiresAt = &v
	}
	if t.LastUsedAt != nil {
		v := timeOf(*t.LastUsedAt)
		out.LastUsedAt = &v
	}
	if t.RevokedAt != nil {
		v := timeOf(*t.RevokedAt)
		out.RevokedAt = &v
	}
	return out
}

// createdTokenWire 创建响应：元数据 + 一次性明文。
func createdTokenWire(t *store.APIToken, plain string) CreatedToken {
	out := CreatedToken{
		Id:        idOf(t.ID),
		Name:      t.Name,
		Prefix:    t.Prefix,
		Kind:      CreatedTokenKind(t.Kind),
		Scopes:    strings.Fields(t.Scopes),
		Token:     plain,
		CreatedAt: timeOf(t.CreatedAt),
	}
	if t.ExpiresAt != nil {
		v := timeOf(*t.ExpiresAt)
		out.ExpiresAt = &v
	}
	return out
}
