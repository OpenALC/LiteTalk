package api

import (
	"errors"
	"net/http"
)

// authMux 注册路由时按生成的 PublicRoutePatterns 决定是否包 auth 中间件。
// 鉴权发生在路径参数解析与请求体解码之前，与旧 s.auth(s.handleXxx) 的
// 调用顺序一致；未列入免鉴权表的路由一律要求 Bearer（fail-closed）。
type authMux struct {
	*http.ServeMux
	s *Server
}

func (a *authMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	// contentTypeFixer 包在最外层：生成 Visit 写 application/json，线上口径带 charset。
	wrap := func(w http.ResponseWriter, r *http.Request) {
		h(contentTypeFixer{w}, r)
	}
	if PublicRoutePatterns[pattern] {
		a.ServeMux.HandleFunc(pattern, wrap)
		return
	}
	a.ServeMux.HandleFunc(pattern, a.s.auth(wrap))
}

// limitBody 请求体 1MB 上限（与旧 decodeJSON 一致）；超限在解码报错里区分。
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// contentTypeFixer 生成代码的 Visit 写 application/json，线上口径带 charset。
// 只在 WriteHeader 前改写，writeJSON 自己带 charset 的路径不受影响。
type contentTypeFixer struct {
	http.ResponseWriter
}

func (c contentTypeFixer) WriteHeader(code int) {
	h := c.ResponseWriter.Header()
	if h.Get("Content-Type") == "application/json" {
		h.Set("Content-Type", "application/json; charset=utf-8")
	}
	c.ResponseWriter.WriteHeader(code)
}

// requestErrorHandler 与旧 decodeJSON 逐字一致：超限 413 too_large，
// 其余 400 bad_request「请求体不是合法 JSON」。
func (s *Server) requestErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "请求体过大")
		return
	}
	writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
}

// responseErrorHandler 统一写出 strict 方法返回的 *apiErr；未知错误按 500 记日志。
func (s *Server) responseErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	var e *apiErr
	if errors.As(err, &e) {
		if e.header != "" {
			w.Header().Set("X-LiteTalk-Auth", e.header)
		}
		writeErr(w, e.status, e.code, e.message)
		return
	}
	s.log.Printf("[500] %s: %v", r.URL.Path, err)
	writeErr(w, http.StatusInternalServerError, "internal", "服务器内部错误")
}
