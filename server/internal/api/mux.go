package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// clientVersionHeader 客户端版本头（#31 强制升级门）。
// v1.5 起 REST 的 ID 一律字符串、错误体 detail 取代 message，v1.1 客户端
// 解码 int64 ID 会失败且读不到错误提示，必须在 /auth/* 前挡住并给出升级提示。
const clientVersionHeader = "X-LiteTalk-Client"

// 允许通过升级门的最低客户端版本（主/次）。补丁号与 -alpha/-beta 后缀不参与比较。
const (
	minClientMajor = 1
	minClientMinor = 5
)

// clientTooOldMessage 426 提示文案：旧客户端会把 message 原样显示出来（#31）。
const clientTooOldMessage = "客户端版本过旧，请升级到 v1.5"

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
		// 升级门在鉴权/解码之前：旧客户端连请求体都解不对，先给明确提示。
		if clientVersionGated(r.URL.Path) && !clientVersionAccepted(r.Header.Get(clientVersionHeader)) {
			writeErr(w, http.StatusUpgradeRequired, "client_too_old", clientTooOldMessage)
			return
		}
		h(contentTypeFixer{w}, r)
	}
	if PublicRoutePatterns[pattern] {
		a.ServeMux.HandleFunc(pattern, wrap)
		return
	}
	a.ServeMux.HandleFunc(pattern, a.s.auth(wrap))
}

// clientVersionGated 指定路径是否受强制升级门约束（#31）。
// 只拦 /auth/* 下的登录相关端点；logout 豁免，否则旧客户端连吊销令牌都做不到。
// healthz 与其它端点不拦：旧客户端的连接测试必须仍能探活。
func clientVersionGated(path string) bool {
	if !strings.HasPrefix(path, "/api/v1/auth/") {
		return false
	}
	return path != "/api/v1/auth/logout"
}

// clientVersionAccepted 解析 X-LiteTalk-Client 并判定是否达到最低版本。
//   - 缺失 → 拒绝（fail-closed）；
//   - dev（任意大小写）或包含 dev 的探测值 → 放行（本地构建 / e2eprobe / 契约模糊测试）；
//   - 其余按主/次版本解析：主 > 1，或主 = 1 且次 >= 5 放行；无法解析 → 拒绝。
//
// 前缀 v 与 -alpha/-beta 后缀忽略（v1.5.0-alpha 可解析）；后缀不参与比较，
// 否则预发布客户端会被自己挡住。
func clientVersionAccepted(header string) bool {
	v := strings.TrimSpace(header)
	if v == "" {
		return false
	}
	lower := strings.ToLower(v)
	if lower == "dev" || strings.Contains(lower, "dev") {
		return true
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	parts := strings.Split(v, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor := 0
	if len(parts) > 1 {
		// 第三段可能带 -alpha 等后缀，到这里已经不需要，只解析前两段。
		if minor, err = strconv.Atoi(parts[1]); err != nil {
			return false
		}
	}
	return major > minClientMajor || (major == minClientMajor && minor >= minClientMinor)
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
