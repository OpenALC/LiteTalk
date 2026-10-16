// Package api REST API 层（§5.4 全量接口）。路由用标准库 ServeMux（Go 1.22 模式匹配）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/mailer"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// AuthMode 鉴权模式：full（默认，验证码流程）/ simple（免验证码，适合私域部署）。
const (
	AuthModeFull   = "full"
	AuthModeSimple = "simple"
)

// Server API 依赖集合。
type Server struct {
	store    store.Store
	jwt      *auth.JWT
	refresh  *auth.RefreshStore
	codes    *auth.EmailCodes
	limits   *auth.RateLimit
	mailer   mailer.Mailer
	authMode string
	log      *log.Logger
	trusted  []*net.IPNet // 可信反代（RemoteAddr 命中才采用 X-Real-IP）
}

// Config 构造配置。
type Config struct {
	Store    store.Store
	JWT      *auth.JWT
	Refresh  *auth.RefreshStore
	Codes    *auth.EmailCodes
	Limits   *auth.RateLimit
	Mailer   mailer.Mailer
	AuthMode string // 空视为 full
	Log      *log.Logger
	// TrustedProxies 可信反代的 IP/CIDR 列表（逗号分隔 env 在 main 解析后传入）。
	// 空 = 不信任任何代理：直接取 RemoteAddr，伪造 X-Real-IP 无法绕过限流（P1）。
	TrustedProxies []string
}

// New 构造 API Server。
func New(cfg Config) *Server {
	lg := cfg.Log
	if lg == nil {
		lg = log.Default()
	}
	m := cfg.Mailer
	if m == nil {
		m = &mailer.Console{Log: lg}
	}
	mode := cfg.AuthMode
	if mode != AuthModeSimple {
		mode = AuthModeFull
	}
	return &Server{
		store:    cfg.Store,
		jwt:      cfg.JWT,
		refresh:  cfg.Refresh,
		codes:    cfg.Codes,
		limits:   cfg.Limits,
		mailer:   m,
		authMode: mode,
		log:      lg,
		trusted:  parseTrustedProxies(lg, cfg.TrustedProxies),
	}
}

// parseTrustedProxies 把 IP/CIDR 字符串列表解析成匹配网络；非法项跳过并记日志。
func parseTrustedProxies(lg *log.Logger, entries []string) []*net.IPNet {
	var out []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if _, n, err := net.ParseCIDR(e); err == nil {
				out = append(out, n)
			} else {
				lg.Printf("[config] 忽略非法可信代理 CIDR %q: %v", e, err)
			}
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 128 // IPv6 按 /128 精确匹配
			if ip4 := ip.To4(); ip4 != nil {
				ip, bits = ip4, 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		} else {
			lg.Printf("[config] 忽略非法可信代理 IP %q", e)
		}
	}
	return out
}

// simpleMode 当前是否为简捷模式（免验证码）。
func (s *Server) simpleMode() bool { return s.authMode == AuthModeSimple }

// mailConfigured 发信通道是否为真实投递（Console 打印不算）。
func (s *Server) mailConfigured() bool {
	_, isConsole := s.mailer.(*mailer.Console)
	return !isConsole
}

// Handler 返回挂载全部路由的 http.Handler（前缀 /api/v1）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// config（免鉴权，客户端据此适配 UI）
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	// auth
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/guest", s.handleGuest)
	mux.HandleFunc("POST /api/v1/auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/v1/auth/email-code", s.handleEmailCode)
	mux.HandleFunc("POST /api/v1/auth/password/reset", s.handlePasswordReset)
	// users
	mux.HandleFunc("GET /api/v1/users/me", s.auth(s.handleMe))
	mux.HandleFunc("PATCH /api/v1/users/me", s.auth(s.handlePatchMe))
	// friends
	mux.HandleFunc("GET /api/v1/friends", s.auth(s.handleListFriends))
	mux.HandleFunc("POST /api/v1/friends/requests", s.auth(s.handleFriendRequest))
	mux.HandleFunc("POST /api/v1/friends/requests/{id}/accept", s.auth(s.handleFriendAccept))
	mux.HandleFunc("DELETE /api/v1/friends/{id}", s.auth(s.handleFriendDelete))
	mux.HandleFunc("PATCH /api/v1/friends/{id}", s.auth(s.handlePatchFriend))
	// servers
	mux.HandleFunc("POST /api/v1/servers", s.auth(s.handleCreateServer))
	mux.HandleFunc("GET /api/v1/servers", s.auth(s.handleListServers))
	mux.HandleFunc("PATCH /api/v1/servers/{id}", s.auth(s.handlePatchServer))
	mux.HandleFunc("DELETE /api/v1/servers/{id}", s.auth(s.handleDeleteServer))
	mux.HandleFunc("POST /api/v1/servers/join", s.auth(s.handleJoinServer))
	mux.HandleFunc("GET /api/v1/servers/{id}/members", s.auth(s.handleServerMembers))
	mux.HandleFunc("POST /api/v1/servers/{id}/invites", s.auth(s.handleCreateInvite))
	// channels
	mux.HandleFunc("POST /api/v1/servers/{id}/channels", s.auth(s.handleCreateChannel))
	mux.HandleFunc("GET /api/v1/servers/{id}/channels", s.auth(s.handleListChannels))
	mux.HandleFunc("PATCH /api/v1/channels/{id}", s.auth(s.handlePatchChannel))
	mux.HandleFunc("DELETE /api/v1/channels/{id}", s.auth(s.handleDeleteChannel))
	// categories（v0.1.2 迁移 0003）
	mux.HandleFunc("POST /api/v1/servers/{id}/categories", s.auth(s.handleCreateCategory))
	mux.HandleFunc("DELETE /api/v1/categories/{id}", s.auth(s.handleDeleteCategory))
	// messages
	mux.HandleFunc("GET /api/v1/channels/{id}/messages", s.auth(s.handleListMessages))
	mux.HandleFunc("POST /api/v1/channels/{id}/messages", s.auth(s.handleCreateMessage))
	return mux
}

// ---- 上下文 ----

type ctxKey int

const userKey ctxKey = 1

// currentUser 从请求上下文取当前用户（auth 中间件注入）。
func currentUser(r *http.Request) *auth.Claims {
	c, _ := r.Context().Value(userKey).(*auth.Claims)
	return c
}

// auth 鉴权中间件：Authorization: Bearer <access_jwt>。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		token := strings.TrimPrefix(h, "Bearer ")
		if token == "" || token == h {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "缺少访问令牌")
			return
		}
		claims, err := s.jwt.ParseAccess(token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "访问令牌无效或已过期")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, claims)))
	}
}

// ---- 响应工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError 统一错误体 {"code","message"}。
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

// writeStoreErr 把 store 领域错误映射为 HTTP。
func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", "资源冲突")
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", "无权操作")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// decodeJSON 读取并校验请求体。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength > 1<<20 { // 1MB 上限
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "请求体过大")
		return false
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return false
	}
	return true
}

// pathID 读取路径参数 {id}。
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "路径参数不合法")
		return 0, false
	}
	return id, true
}

// clientIP 取客户端 IP：仅当 RemoteAddr 命中可信代理列表时才采用 X-Real-IP。
// 默认（空列表）不信任任何代理——服务直接监听公网时，无条件信任 X-Real-IP
// 会让伪造该头绕过登录/注册等 IP 限流（P1）。
// IPv4/IPv6 统一用 net.SplitHostPort 解析：按 ":" 切会把 IPv6 截成 "["，
// 导致全部 IPv6 用户共用一个限流桶。
func (s *Server) clientIP(r *http.Request) string {
	host := remoteHost(r.RemoteAddr)
	if remoteIP := net.ParseIP(host); remoteIP != nil {
		for _, n := range s.trusted {
			if n.Contains(remoteIP) {
				if x := strings.TrimSpace(r.Header.Get("X-Real-IP")); x != "" {
					if ip := net.ParseIP(x); ip != nil {
						return ip.String()
					}
				}
				break
			}
		}
	}
	return host
}

// remoteHost 从 RemoteAddr（host:port / [v6]:port）提取 host；解析失败原样返回。
func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
