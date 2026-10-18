// Package api REST API 层（§5.4 全量接口）。路由用标准库 ServeMux（Go 1.22 模式匹配）。
package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/mailer"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// AuthMode 鉴权模式：full（默认，验证码流程）/ simple（免验证码，适合私域部署）。
const (
	AuthModeFull   = "full"
	AuthModeSimple = "simple"
)

// Hub abstracts signal.Hub for logout/reset-password session kicking.
// Defined here to avoid an import cycle (signal imports store, api imports signal would cycle via main wiring).
type Hub interface {
	Kick(uid, dev, reason string)
	KickChannel(channelID, reason string)
}

// PresenceReader 在线状态读取（#11）：presence:{uid} 存在 = 在线。
// store.KV 满足；nil 时 user.online 恒 false。键格式与 signal 包手工同步。
type PresenceReader interface {
	Get(ctx context.Context, key string) (string, error)
}

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
	trusted  []*net.IPNet       // 可信反代（RemoteAddr 命中才采用 X-Real-IP）
	sess     *auth.SessionIndex // 多设备会话索引（v1.1.0 迁移 0004）
	hub      Hub                // WS 信令 hub（登出/重置密码踢线；nil=不踢）
	presence PresenceReader     // 在线状态（好友列表 user.online 用；nil=恒离线）
	selfHost bool               // 一键自托管形态（GET /api/v1/config 自描述，#26）
	// bootstrapRequired 管理员邀请码是否未消费（#27 T4 接线；nil=恒 false）。
	bootstrapRequired func() bool
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
	// Session 多设备会话索引（nil 时跳过 sid 校验，仅供测试/迁移期过渡）。
	Session *auth.SessionIndex
	// Hub 信令 hub（登出/重置密码踢线；nil 时仅清会话不踢 WS，测试常用）。
	Hub Hub
	// Presence 在线状态读取（presence:{uid}，好友列表 user.online 填值用；nil=恒离线）。
	Presence PresenceReader
	// SelfHost 一键自托管形态（SQLite/进程内单机部署）：客户端显示自托管引导（#26）。
	SelfHost bool
	// BootstrapRequired 管理员邀请码是否尚未被消费；注册页提示「等待管理员邀请」。
	// nil=恒 false（bootstrap 流程未启用，#27 T4 接线）。
	BootstrapRequired func() bool
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
		store:             cfg.Store,
		jwt:               cfg.JWT,
		refresh:           cfg.Refresh,
		codes:             cfg.Codes,
		limits:            cfg.Limits,
		mailer:            m,
		authMode:          mode,
		log:               lg,
		trusted:           parseTrustedProxies(lg, cfg.TrustedProxies),
		sess:              cfg.Session,
		hub:               cfg.Hub,
		presence:          cfg.Presence,
		selfHost:          cfg.SelfHost,
		bootstrapRequired: cfg.BootstrapRequired,
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
// 路由表由 api.gen.go 按 openapi.yaml 注册；authMux 在注册时对非免鉴权
// 路由包 auth 中间件（顺序与旧 s.auth(s.handleXxx) 一致）。
func (s *Server) Handler() http.Handler {
	si := NewStrictHandlerWithOptions(s, []StrictMiddlewareFunc{s.withStrictCtx}, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  s.requestErrorHandler,
		ResponseErrorHandlerFunc: s.responseErrorHandler,
	})
	return HandlerWithOptions(si, StdHTTPServerOptions{
		BaseRouter:  &authMux{ServeMux: http.NewServeMux(), s: s},
		Middlewares: []MiddlewareFunc{limitBody},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadRequest, "bad_request", "路径参数不合法")
		},
	})
}

// ---- 上下文 ----

type ctxKey int

const userKey ctxKey = 1

// legacyTokenGrace 旧版本令牌（无 sid）过渡期：与 signal 侧同值同时长，
// 服务端升级后已签发令牌在有效期内仍可用，给用户留重新登录窗口（P1-3）。
const legacyTokenGrace = 2*time.Hour + 5*time.Minute

// auth 鉴权中间件：Authorization: Bearer <access_jwt>。
// 多设备会话（v1.1.0）：比对 claims.Sid 与当前会话 sid；不符 → 401 session_replaced。
// 旧版本令牌（sid 空）在 legacyTokenGrace 过渡期内放行（兼容 v1.0.x 客户端）；
// sess 为 nil 时整段跳过（测试/降级路径，生产不可达）。
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
		if s.sess != nil {
			if claims.Sid == "" {
				// 旧版本令牌：过渡期内放行，超期拒绝。
				if claims.IssuedAt == nil || time.Since(claims.IssuedAt.Time) >= legacyTokenGrace {
					w.Header().Set("X-LiteTalk-Auth", "session_replaced")
					writeErr(w, http.StatusUnauthorized, "session_replaced", "登录状态已过期，请重新登录")
					return
				}
			} else {
				curSid, err := s.sess.Get(r.Context(), claims.UserID, claims.Dev)
				if err != nil || curSid == "" || claims.Sid != curSid {
					w.Header().Set("X-LiteTalk-Auth", "session_replaced")
					writeErr(w, http.StatusUnauthorized, "session_replaced", "账号已在其他设备登录")
					return
				}
			}
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

// writeErr 统一错误体 {"code","message"}（契约 ApiError）。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ApiError{Code: code, Message: msg})
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
