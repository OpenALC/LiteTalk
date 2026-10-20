package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
func (s *Server) writeStoreErr(ctx context.Context, err error) *apiErr {
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
	respWriterKey
	httpReqKey
)

// withStrictCtx 把请求路径、客户端 IP、响应写入器与原始请求放进 ctx（strict 方法
// 拿不到 *http.Request / ResponseWriter；限流头与 If-None-Match 协商要用，#31）。
func (s *Server) withStrictCtx(f StrictHandlerFunc, _ string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		ctx = context.WithValue(ctx, reqPathKey, r.URL.Path)
		ctx = context.WithValue(ctx, clientIPKey, s.clientIP(r))
		ctx = context.WithValue(ctx, respWriterKey, w)
		ctx = context.WithValue(ctx, httpReqKey, r)
		return f(ctx, w, r, request)
	}
}

// ifNoneMatch 取请求的 If-None-Match（ETag 协商，#31）；无则空串。
func ifNoneMatch(ctx context.Context) string {
	r, _ := ctx.Value(httpReqKey).(*http.Request)
	if r == nil {
		return ""
	}
	return r.Header.Get("If-None-Match")
}

// etagMatch 判断 If-None-Match 是否命中当前 ETag：支持 *、W/ 弱前缀与逗号列表。
func etagMatch(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || part == etag || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// setETag 写响应 ETag（#31 协商缓存）。
func setETag(ctx context.Context, etag string) {
	w, _ := ctx.Value(respWriterKey).(http.ResponseWriter)
	if w != nil {
		w.Header().Set("ETag", etag)
	}
}

// setRateLimit 写 X-RateLimit-Limit/Remaining/Reset（#31 限流头）；
// retryAfter>0 时补 Retry-After（秒）。reset 为窗口重置的 unix 秒（上界）。
func setRateLimit(ctx context.Context, limit, remaining int, reset time.Time, retryAfter time.Duration) {
	w, _ := ctx.Value(respWriterKey).(http.ResponseWriter)
	if w == nil {
		return
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
	}
}

// allowRate 限流判定并写 X-RateLimit-*（#31）。超限补 Retry-After 并返回 429
// （KV 故障同样 429，限流 fail-closed 与迁移前一致）。
func (s *Server) allowRate(ctx context.Context, key string, limit int, window time.Duration, msg string) *apiErr {
	ok, remaining, reset, err := s.limits.AllowInfo(ctx, key, limit, window)
	setRateLimit(ctx, limit, remaining, reset, 0)
	if err != nil || !ok {
		setRateLimit(ctx, limit, 0, reset, window)
		return errf(http.StatusTooManyRequests, "rate_limited", msg)
	}
	return nil
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
