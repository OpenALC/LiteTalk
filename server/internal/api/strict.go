package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// apiErr 业务错误载体：strict 方法返回它，由 ResponseErrorHandlerFunc 统一经
// writeErr 写出。错误码/状态码/文案与迁移前逐字一致，不随重构改语义。
// header 非空时补写 X-LiteTalk-Auth（session_replaced / session_replayed）。
type apiErr struct {
	status  int
	code    string
	message string
	header  string
}

func (e *apiErr) Error() string { return e.code + ": " + e.message }

// errf 构造业务错误。
func errf(status int, code, msg string) *apiErr {
	return &apiErr{status: status, code: code, message: msg}
}

// writeStoreErr 把 store 领域错误映射为 *apiErr（500 记底层错误与路径）。
// strict 迁移后方法签名改为返回错误对象——它是方法不是自由函数，陷阱不变。
func (s *Server) writeStoreErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errf(http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, store.ErrConflict):
		return errf(http.StatusConflict, "conflict", "资源冲突")
	case errors.Is(err, store.ErrForbidden):
		return errf(http.StatusForbidden, "forbidden", "无权操作")
	case errors.Is(err, store.ErrExpired):
		return errf(http.StatusGone, "code_expired", "邀请码已过期")
	default:
		s.log.Printf("[500] %s: %v", reqPath(ctx), err)
		return errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

type strictCtxKey int

const (
	reqPathKey strictCtxKey = iota + 1
	clientIPKey
)

// withStrictCtx 把请求路径与客户端 IP 放进 ctx（strict 方法拿不到 *http.Request）。
func (s *Server) withStrictCtx(f StrictHandlerFunc, _ string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		ctx = context.WithValue(ctx, reqPathKey, r.URL.Path)
		ctx = context.WithValue(ctx, clientIPKey, s.clientIP(r))
		return f(ctx, w, r, request)
	}
}

func reqPath(ctx context.Context) string {
	p, _ := ctx.Value(reqPathKey).(string)
	return p
}

// reqIP 解析后的客户端 IP（可信代理判定在注入时已做）。
func reqIP(ctx context.Context) string {
	p, _ := ctx.Value(clientIPKey).(string)
	return p
}

// currentUser 从上下文取当前用户（auth 中间件注入）。
func currentUser(ctx context.Context) *auth.Claims {
	c, _ := ctx.Value(userKey).(*auth.Claims)
	return c
}

// checkID 路径 ID 须为正整数（沿用旧 pathID 口径；非数字由 adapter 层先拒）。
func checkID(id int64) *apiErr {
	if id <= 0 {
		return errf(http.StatusBadRequest, "bad_request", "路径参数不合法")
	}
	return nil
}

// bodyMissing 空请求体按旧 decodeJSON 口径拒绝（EOF 同样 400）。
func bodyMissing() *apiErr {
	return errf(http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
}
