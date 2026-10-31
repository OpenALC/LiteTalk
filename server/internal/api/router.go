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

	"github.com/OpenALC/LiteTalk/server/internal/attachment"
	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/authz"
	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/linkpreview"
	"github.com/OpenALC/LiteTalk/server/internal/mailer"
	"github.com/OpenALC/LiteTalk/server/internal/push"
	"github.com/OpenALC/LiteTalk/server/internal/rtcapi"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
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
	// SetServerMuted 服务端强制闭麦/解禁（#36）：目标不在该频道返回 signal.ErrNotInChannel。
	SetServerMuted(channelID, userID string, muted bool) error
	// BroadcastConversationChanged 会话变更通知（#38 B3）：uids 为十进制用户 ID，
	// 动作见 signal.ConversationChangedData.Action。空列表不广播。
	BroadcastConversationChanged(uids []string, conversationID, action string)
	// BroadcastMessageEdited 消息编辑广播（#39）：uids 为十进制用户 ID（调用方已按
	// 频道可见性过滤）；editedAt 为 Unix 毫秒。
	BroadcastMessageEdited(uids []string, channelID, messageID, content string, editedAt int64, mentions []string)
	// BroadcastMessageDeleted 消息软删除广播（#39）：目标口径同上。
	BroadcastMessageDeleted(uids []string, channelID, messageID string)
	// BroadcastChannelUnread 未读计数变更（#40）：counts 按十进制 uid 给每个
	// 接收者自己的精确计数，只推给该用户（不进其他人的事件环）。空 map 不广播。
	BroadcastChannelUnread(channelID string, counts map[string]store.Unread)
	// BroadcastChannelAudio 语音频道音质变更（#46）：uids 为十进制用户 ID
	//（调用方已按频道 ViewChannel 可见性过滤）；0 表示跟随设置/默认帧长。
	BroadcastChannelAudio(uids []string, channelID string, bitrateKbps, frameMS int)
	// BroadcastVoicePriority 优先发言状态变更（#45）：调用方只在频道覆盖变更后
	// 对 member 目标调用；实现只发给该语音房成员的全部在线连接（协议 ≥3）。
	// 角色/成员角色变更不实时广播，重进房由 channel.joined 的 MemberInfo.priority
	// 刷新，因此本方法不覆盖那些路径。
	BroadcastVoicePriority(channelID, userID string, priority bool)
	// RoomHasLegacyClients 该房间当前是否有协议 <3 的连接（#116）：PATCH 启用
	// 10ms 帧长前检查，避免与不认识 10ms 的旧客户端混用；房间不存在返回 false。
	RoomHasLegacyClients(channelID string) bool
}

// PresenceReader 在线状态读取（#11）：presence:{uid} 存在 = 在线。
// store.KV 满足；nil 时 user.online 恒 false。键格式与 signal 包手工同步。
type PresenceReader interface {
	Get(ctx context.Context, key string) (string, error)
}

// VoicePresence 好友频道 presence 反查（#92）：uid → 当前语音房。
// *signal.Hub 满足（VoiceChannelOf）；窄接口单独注入，不塞进 Hub——
// api.Hub 只管踢线，fakeHub 不必为好友快照补方法。
type VoicePresence interface {
	// VoiceChannelOf 返回 uid 当前语音房 ID 与所属服务器 ID（均为十进制字符串）。
	// dev 内置房间 serverID 为 "0"（无 store 频道实体，调用方不带频道字段）。
	VoiceChannelOf(uid string) (channelID, serverID string, ok bool)
}

// LinkPreviewFetcher 链接预览抓取器（#42 B4）。生产传 *linkpreview.Fetcher，
// 测试注入桩。nil = 功能关闭：GET /api/v1/link-preview 一律 404 no_preview。
type LinkPreviewFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*linkpreview.Preview, error)
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
	voice    VoicePresence      // 语音房反查（好友频道 presence 快照，#92；nil=不带频道字段）
	selfHost bool               // 一键自托管形态（GET /api/v1/config 自描述，#26）
	// authz 权限判定（#35 B3）：与 signal.Hub 共享同一实例，缓存失效才同步。
	authz *authz.Authorizer
	// apiTokens api_token（pat/bot）校验服务（#54）：REST 中间件与 WS 握手共用
	// 同一条实现。nil 时按 Store 自建（测试兼容）；生产传与 WS 同一实例。
	apiTokens *token.Service
	// bootstrapRequired 管理员邀请码是否未消费（#27 T4 接线；nil=恒 false）。
	bootstrapRequired func() bool
	// version 服务端构建版本（GET /api/v1/healthz 回显，#77）。
	version string
	// rtcCfg TURN/STUN 凭证签发配置（POST /api/v1/rtc/credentials，#77）。
	rtcCfg rtcapi.Config
	// rtcAuth 从访问令牌提取 uid；nil 时该端点一律 401（fail-closed）。
	rtcAuth rtcapi.UserFromToken
	// blob 附件二进制存储（#41 B2）：上传/下载端点用；nil 时附件端点 500（测试/降级）。
	blob blob.Store
	// upload 附件校验参数（MaxBytes/MaxDimension/AllowedTypes）；New 里补默认值。
	upload attachment.Config
	// kv 共享键值存储（链接预览缓存，#42 B4）；nil 时链接预览按不可用处理。
	kv store.KV
	// linkPreview 链接预览抓取器（nil=功能关闭，端点统一 404 no_preview）。
	linkPreview LinkPreviewFetcher
	// push 移动推送派发器（#53）。nil = 推送关闭：/config 报 push_enabled=false，
	// 触发点全部短路（设备注册端点仍可用，注册数据等开启后即生效）。
	push *push.Broker
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
	// VoicePresence 语音房反查（好友频道 presence 快照，#92）；nil=好友条目不带频道字段。
	// 生产传与 signal.Hub 同一实例（*signal.Hub 满足本接口）。
	VoicePresence VoicePresence
	// SelfHost 一键自托管形态（SQLite/进程内单机部署）：客户端显示自托管引导（#26）。
	SelfHost bool
	// BootstrapRequired 管理员邀请码是否尚未被消费；注册页提示「等待管理员邀请」。
	// nil=恒 false（bootstrap 流程未启用，#27 T4 接线）。
	BootstrapRequired func() bool
	// Version 服务端构建版本（GET /api/v1/healthz 回显；空视为 dev，#77）。
	Version string
	// RTC TURN/STUN 凭证签发配置；Secret 为空时 rtc/credentials 返回 503（#77）。
	RTC rtcapi.Config
	// RTCAuth 从访问令牌提取 uid（Bearer 或 ?token=，与信令一致）；nil 一律 401。
	RTCAuth rtcapi.UserFromToken
	// Authz 权限判定（#35 B3）。nil 时用 Store 自建默认实例（测试兼容）；
	// 生产必须传入与 signal.Hub 共享的同一实例，否则写后失效不同步。
	Authz *authz.Authorizer
	// Tokens api_token 校验服务（#54）。nil 时用 Store 自建（测试兼容）；
	// 生产建议显式传入与 WS 鉴权共用的同一实例（last_used 节流表按实例记）。
	Tokens *token.Service
	// Blob 附件二进制存储（#41 B2）。生产由 cmd/server 按 LITETALK_UPLOAD_* 构造；
	// nil 时上传/下载端点返回 500（测试未注入 blob 的旧用例不受影响）。
	Blob blob.Store
	// Upload 附件校验参数。MaxBytes<=0 回落 10 MiB；MaxDimension<=0 回落 4096
	// （服务端显式收紧，包默认 8192 对解压炸弹风险偏高）；AllowedTypes 为空用默认白名单。
	Upload attachment.Config
	// KV 共享键值存储（链接预览缓存，#42 B4）。nil 时链接预览按不可用处理
	// （端点 404 no_preview）；缓存读写故障则 fail-open，跳过缓存继续抓取。
	KV store.KV
	// LinkPreview 链接预览抓取器（#42 B4）。生产由 cmd/server 按
	// LITETALK_LINK_PREVIEW* 构造；nil = 功能关闭，端点一律 404 no_preview。
	LinkPreview LinkPreviewFetcher
	// Push 移动推送派发器（#53）。生产由 cmd/server 按 LITETALK_PUSH_* 构造；
	// nil = 推送关闭（/config push_enabled=false，触发点短路，零后台 goroutine）。
	Push *push.Broker
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
	ver := cfg.Version
	if ver == "" {
		ver = "dev"
	}
	az := cfg.Authz
	if az == nil && cfg.Store != nil {
		// 测试/单用方场景自建；生产（main.go）注入与 Hub 共享的实例。
		az = authz.New(cfg.Store, authz.Options{})
	}
	toks := cfg.Tokens
	if toks == nil && cfg.Store != nil {
		// 同上：测试/单用方场景自建；生产传入与 WS 鉴权共用的同一实例。
		toks = token.NewService(cfg.Store, lg)
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
		voice:             cfg.VoicePresence,
		selfHost:          cfg.SelfHost,
		authz:             az,
		apiTokens:         toks,
		bootstrapRequired: cfg.BootstrapRequired,
		version:           ver,
		rtcCfg:            cfg.RTC,
		rtcAuth:           cfg.RTCAuth,
		blob:              cfg.Blob,
		upload:            normalizeUploadConfig(cfg.Upload),
		kv:                cfg.KV,
		linkPreview:       cfg.LinkPreview,
		push:              cfg.Push,
	}
}

// serverDefaultMaxDimension 服务端图片单边上限（像素）。attachment 包默认 8192，
// 服务端收紧到 4096：解码巨图的内存峰值与解压炸弹风险都随之下降。
const serverDefaultMaxDimension = 4096

// normalizeUploadConfig 补零值：单文件上限、图片单边上限与白名单都必须有值，
// 否则 limitBody 会按 0 计算上传配额、下载白名单校验也会失效。
func normalizeUploadConfig(cfg attachment.Config) attachment.Config {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = attachment.DefaultMaxBytes
	}
	if cfg.MaxDimension <= 0 {
		cfg.MaxDimension = serverDefaultMaxDimension
	}
	if len(cfg.AllowedTypes) == 0 {
		cfg.AllowedTypes = append([]string(nil), attachment.DefaultAllowedTypes...)
	}
	return cfg
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
	h := HandlerWithOptions(si, StdHTTPServerOptions{
		BaseRouter:  &authMux{ServeMux: http.NewServeMux(), s: s},
		Middlewares: []MiddlewareFunc{s.limitBody},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadRequest, "bad_request", "路径参数不合法")
		},
	})
	// ServeMux 的 405 由 net/http 内部直接 http.Error 写出（纯文本），没有可插拔
	// 的错误处理点；包一层响应改写，保证 405 也是 RFC 9457 problem+json（#77）。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&methodNotAllowedWriter{ResponseWriter: w}, r)
	})
}

// ---- 上下文 ----

// legacyTokenGrace 旧版本令牌（无 sid）过渡期：与 signal 侧同值同时长，
// 服务端升级后已签发令牌在有效期内仍可用，给用户留重新登录窗口（P1-3）。
const legacyTokenGrace = 2*time.Hour + 5*time.Minute

// auth 鉴权中间件：Authorization: Bearer <access_jwt | ltk_pat_...|ltk_bot_...>。
// api_token（#54）按明文前缀分流：ltk_ 开头走 token.Service 校验，**先于** JWT
// 解析判定——别让 JWT 解析器对非法串抛错刷日志。两条路径产出同一个
// token.Principal（内嵌 auth.Claims），handler 与 authz 的口径不变。
// 多设备会话（v1.1.0）：比对 claims.Sid 与当前会话 sid；不符 → 401 session_replaced。
// 旧版本令牌（sid 空）在 legacyTokenGrace 过渡期内放行（兼容 v1.0.x 客户端）；
// sess 为 nil 时整段跳过（测试/降级路径，生产不可达）。
// 令牌路径不做会话比对：api_token 没有会话概念，吊销/过期即失效
// （每次实时查库，绝不缓存 Principal），属主被删/停用同样失效。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		bearer := strings.TrimPrefix(h, "Bearer ")
		if bearer == "" || bearer == h {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "缺少访问令牌")
			return
		}
		var principal *token.Principal
		if strings.HasPrefix(bearer, token.PlainPrefix) {
			// api_token（pat/bot）：独立于密码登录的凭据路径，不进 JWT 解析。
			if s.apiTokens == nil { // 无 Store 的极端构造（fail-closed）
				writeErr(w, http.StatusUnauthorized, "unauthorized", "访问令牌无效或已过期")
				return
			}
			p, err := s.apiTokens.Verify(r.Context(), bearer)
			if err != nil {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "访问令牌无效或已过期")
				return
			}
			principal = p
		} else {
			claims, err := s.jwt.ParseAccess(bearer)
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
			principal = &token.Principal{Claims: claims, Kind: token.KindJWT}
		}
		next(w, r.WithContext(token.WithPrincipal(r.Context(), principal)))
	}
}

// ---- 响应工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// problemBody RFC 9457 问题详情（#31）：type/title/status/code/detail/message。
// code 是扩展字段（客户端 APIError.Code 依赖），字符串码与迁移前逐字一致；
// title 随 status 定（不随实例变），detail 才是本次出错的中文提示。
//
// message 与 detail 同值：RFC 9457 允许扩展字段，v1.1 客户端 decodeError 只认
// message，缺了它任何错误都解析不出来（过期令牌清不掉）。至少保留一个版本（#31）。
type problemBody struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
}

// writeErr 统一错误体（RFC 9457，application/problem+json，契约 Problem）。
// type 形如 urn:litetalk:error:<code>；签名不变，只改写响应形状。
// detail 与 message 必须同时填：前者是新口径，后者兼容已发布的旧客户端。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemBody{
		Type:    "urn:litetalk:error:" + code,
		Title:   http.StatusText(status),
		Status:  status,
		Code:    code,
		Detail:  msg,
		Message: msg,
	})
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
