// 轻音 LiteTalk 服务端入口
// M3 阶段：healthz + WS 信令（GET /ws）+ 自研 Pion SFU + TURN 短期凭证 + 全量 REST API（账号/社交/组织）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	ossignal "os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/acme"
	"github.com/OpenALC/LiteTalk/server/internal/api"
	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/bootstrap"
	"github.com/OpenALC/LiteTalk/server/internal/mailer"
	"github.com/OpenALC/LiteTalk/server/internal/rtcapi"
	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/turn"
	"github.com/OpenALC/LiteTalk/server/migrations"
)

// version 由构建时 -X main.version= 注入，默认 dev
var version = "dev"

// 默认监听地址，可用环境变量 LITETALK_ADDR 覆盖
const defaultAddr = ":8077"

func main() {
	// 启动序（§0.6）：1 解析 flag/env → 2 校验 → 3 Store+Migrate → 4 KV → 5 SFU
	// → 6 内置 TURN → 7 hub/api/rtcapi/mailer → 8 bootstrap → 9 HTTP → 10 关闭序。
	cfg, err := loadConfig(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	// -domain 解析结果回写环境变量：TURN realm / URL 回填区段仍按 os.Getenv 读取
	//（该区段属 T2，不改），回写使 flag > env 优先级在全部区段生效。
	if cfg.domain != "" {
		os.Setenv("LITETALK_DOMAIN", cfg.domain)
	}

	// dev 模式判定：本地 go run / go build 未注入版本号时 version 为 "dev"；
	// CI 打包始终 -X main.version=<tag> 注入真实版本号，故发布产物一律视为生产模式，
	// 生产模式下内存降级、默认密钥、dev 令牌开关等开发便利项全部拒绝启动。
	dev := version == "dev"

	// ---- 配置校验（§0.6 步骤 2）：JWT secret、SFU IP 拒环回、auth mode、域名形态 ----
	if err := cfg.validate(dev); err != nil {
		log.Fatalf("启动配置校验失败: %v", err)
	}

	// ---- 持久化（Store 选型 + KV；§0.3）----
	// Store：LITETALK_STORE 显式 > LITETALK_MYSQL_DSN 非空 → mysql（向后兼容）> 默认 sqlite。
	// KV：显式 LITETALK_REDIS_ADDR 必须连通（拒绝静默降级）；未配置默认进程内 MemKV（#24）。
	var st store.Store
	var kv store.KV
	switch pickStoreKind() {
	case "mysql":
		dsn := os.Getenv("LITETALK_MYSQL_DSN")
		if dsn == "" {
			log.Fatal("LITETALK_STORE=mysql 但未配置 LITETALK_MYSQL_DSN")
		}
		my, err := store.OpenMySQL(dsn)
		if err != nil {
			log.Fatalf("MySQL 连接失败: %v", err)
		}
		defer my.Close()
		if err := my.Migrate(context.Background(), resolveMigrationsMySQL()); err != nil {
			log.Fatalf("数据库迁移失败: %v", err)
		}
		st = my
		log.Println("持久化: MySQL")
	case "sqlite":
		path := os.Getenv("LITETALK_SQLITE_PATH")
		if path == "" {
			// -data / LITETALK_DATA_DIR 经 config 解析（flag > env > 默认）。
			path = filepath.Join(cfg.dataDir, "litetalk.db")
		}
		sq, err := store.OpenSQLite(path)
		if err != nil {
			log.Fatalf("SQLite 打开失败: %v", err)
		}
		defer sq.Close()
		if err := sq.Migrate(context.Background(), resolveMigrationsSQLite()); err != nil {
			log.Fatalf("数据库迁移失败: %v", err)
		}
		st = sq
		log.Printf("持久化: SQLite（%s）", path)
	case "mem":
		// 内存存储重启即失（用户/服务器/消息全丢），生产拒绝，仅 dev 本地开发。
		if !dev {
			log.Fatal("LITETALK_STORE=mem 仅限 dev 模式：生产请用 sqlite 或 mysql")
		}
		st = store.NewMem()
		log.Println("持久化: 内存（仅 dev）")
	default:
		log.Fatalf("LITETALK_STORE=%q 不支持：可选 mysql / sqlite / mem", pickStoreKind())
	}
	if addr := os.Getenv("LITETALK_REDIS_ADDR"); addr != "" {
		rdb, err := store.OpenRedis(addr)
		if err != nil {
			log.Fatalf("Redis 连接失败（%v）：已配置 LITETALK_REDIS_ADDR，必须可用，拒绝降级内存 KV", err)
		}
		defer rdb.Close()
		kv = rdb
		log.Println("KV: Redis")
	} else {
		// #24 放宽生产 KV 守卫：单机自托管不该被 Redis 缺失挡住启动。
		kv = store.NewMemKV()
		log.Println("KV: 进程内（会话/验证码/限流不跨重启；多实例部署必须配置 LITETALK_REDIS_ADDR）")
	}

	// -print-admin-invite：重新打印管理员邀请码后退出（#27 T4 填实现），不启动 SFU/TURN/HTTP。
	// 需要 KV 判定消费标记，故放在 Store/KV 就绪之后。
	if cfg.printAdminInvite {
		bootstrap.PrintAdminInvite(context.Background(), kv, st, cfg.jwtSecret, log.Default())
		return
	}

	// ---- 账号安全 ----
	// JWT secret 与 SFU IP 校验已并入 cfg.validate（§0.6 步骤 2，持久化之前）。
	jwtSecret := cfg.jwtSecret
	sfuIP := cfg.sfuIP
	jwtSigner := auth.NewJWT(jwtSecret)
	refreshStore := auth.NewRefreshStore(kv)
	codes := auth.NewEmailCodes(kv)
	limits := auth.NewRateLimit(kv)
	sessIndex := auth.NewSessionIndex(st, kv)

	// SFU：ICE-Lite + 单 UDP mux（§5.2）+ 可选 ICE-TCP mux（#25）
	sfuUDPPort := envInt("LITETALK_SFU_UDP", 7845)
	// ICE-TCP 默认与 UDP 同号（各自独立 socket，不是共 socket 复用）；0 = 关闭。
	sfuTCPPort := envInt("LITETALK_SFU_TCP", sfuUDPPort)
	sfuInst, err := sfu.New(sfu.Config{
		UDPPort:      sfuUDPPort,
		TCPPort:      sfuTCPPort,
		NAT1To1IP:    sfuIP,
		MaxMembers:   envInt("LITETALK_SFU_MAX", 10),
		MaxRooms:     envInt("LITETALK_MAX_ROOMS", 50),
		ActiveN:      cfg.sfuActiveN,
		ActiveHoldMS: cfg.sfuActiveHoldMS,
		Logger:       log.Default(),
	})
	if err != nil {
		log.Fatalf("SFU 初始化失败: %v", err)
	}
	defer sfuInst.Close()

	// ---- 内置 TURN（#25）：UDP+TCP 双监听，与 rtcapi 短期凭证互通 ----
	// LITETALK_TURN_PORT=0 关闭（走外部 coturn）；默认 3478。
	turnPort := envInt("LITETALK_TURN_PORT", 3478)
	// 显式配置的密钥先过弱密钥校验（生产模式）；未配置且内置 TURN 启用时随机生成。
	turnSecret, generated, err := resolveTURNSecret(os.Getenv("LITETALK_TURN_SECRET"), turnPort != 0, dev)
	if err != nil {
		log.Fatalf("TURN 凭证密钥不可用: %v", err)
	}
	if generated {
		log.Println("TURN: 未设 LITETALK_TURN_SECRET，已生成随机凭证密钥（重启后旧凭证失效）")
	}
	var turnSrv *turn.Server
	if turnPort != 0 {
		relayMin := uint16(envInt("LITETALK_TURN_RELAY_PORT", 49152))
		relayMax := uint16(envInt("LITETALK_TURN_RELAY_PORT_END", 49200))
		turnSrv, err = turn.New(turn.Config{
			UDPPort:      turnPort,
			TCPPort:      turnPort, // 同号 TCP：禁 UDP 网络的中继兜底（#25 裁定）
			RelayIP:      net.ParseIP(sfuIP),
			RelayMinPort: relayMin,
			RelayMaxPort: relayMax,
			Realm:        envOr("LITETALK_TURN_REALM", envOr("LITETALK_DOMAIN", "litetalk")),
			Secret:       turnSecret,
			Logger:       log.Default(),
		})
		if err != nil {
			log.Fatalf("内置 TURN 启动失败: %v", err)
		}
		defer turnSrv.Close()
	}

	// WS 信令 hub：默认 JWT 鉴权；dev 模式下 LITETALK_DEV_AUTH=1 可用 dev- 前缀令牌兜底（JWT 优先）。
	// dev 令牌把 dev-<名字> 映射为真实身份且不做签名校验，生产构建（注入了版本号）必须拒绝启动。
	devAuth := os.Getenv("LITETALK_DEV_AUTH") == "1"
	if devAuth && !dev {
		log.Fatal("LITETALK_DEV_AUTH=1 仅限 dev 模式（未注入版本号的本地构建）：生产构建拒绝启用 dev 令牌")
	}
	var authenticator signal.Authenticator
	if devAuth {
		authenticator = func(token string) (signal.Identity, error) {
			if !strings.HasPrefix(token, "dev-") {
				if claims, err := jwtSigner.ParseAccess(token); err == nil {
					return identityFromClaims(claims), nil
				}
			}
			// dev- 令牌无会话概念：SkipSessionCheck 跳过握手 sid 比对（仅 dev 构建可达）。
			return signal.DevAuthenticator(token)
		}
		log.Println("WS 鉴权: JWT + dev-token 兜底（开发模式）")
	} else {
		authenticator = func(token string) (signal.Identity, error) {
			claims, err := jwtSigner.ParseAccess(token)
			if err != nil {
				return signal.Identity{}, signal.ErrUnauthorized
			}
			return identityFromClaims(claims), nil
		}
		log.Println("WS 鉴权: JWT")
	}
	hub := signal.NewHub(sfuInst, authenticator, sessIndex, log.Default(), 0, signal.ChatDeps{
		Store:     st,
		Limits:    limits,
		Presence:  presenceDeps{kv: kv, st: st},
		Dev:       dev,
		MaxConns:  envInt("LITETALK_MAX_CONNS", 8),
		JoinRate:  envInt("LITETALK_JOIN_RATE", 30),
		OfferRate: envInt("LITETALK_OFFER_RATE", 60),
		// 每会话事件环条数（#32）：断线续传窗口 = min(环容量, 30min 空闲 TTL)。
		Backlog: cfg.gatewayBacklog,
	})
	// LITETALK_PROTOCOL_MAX：服务端宣布的协议版本上限（#29 逃生阀）。
	// 0/缺省 = 构建期 ProtocolVersionMax；调小可临时挡住过新的客户端区间。
	hub.SetProtoMax(envInt("LITETALK_PROTOCOL_MAX", 0))

	// TURN 短期凭证签发（coturn use-auth-secret，§5.4）
	var rtcAuth rtcapi.UserFromToken
	if devAuth {
		rtcAuth = func(token string) (string, error) {
			if !strings.HasPrefix(token, "dev-") {
				if claims, err := jwtSigner.ParseAccess(token); err == nil {
					return strconv.FormatInt(claims.UserID, 10), nil
				}
			}
			return devUserID(token)
		}
	} else {
		rtcAuth = func(token string) (string, error) {
			claims, err := jwtSigner.ParseAccess(token)
			if err != nil {
				return "", err
			}
			return strconv.FormatInt(claims.UserID, 10), nil
		}
	}
	// TURN 短期凭证签发 URLs（§0.4）：显式 env 优先；未设且内置 TURN 启用则按
	// host（LITETALK_DOMAIN，否则 LITETALK_SFU_IP）回填。host 别一个用 IP 一个用域名。
	turnURLs := splitList(os.Getenv("LITETALK_TURN_URLS"))
	stunURLs := splitList(os.Getenv("LITETALK_STUN_URLS"))
	if len(turnURLs) == 0 || len(stunURLs) == 0 {
		host := envOr("LITETALK_DOMAIN", sfuIP)
		if turnPort != 0 {
			if len(turnURLs) == 0 && len(stunURLs) == 0 {
				turnURLs, stunURLs = rtcapi.DefaultURLs(host, turnPort)
			}
		} else {
			// 内置 TURN 关闭且未显式配置：保持既有开发默认值（仅本机联调可达）。
			if len(turnURLs) == 0 {
				turnURLs = []string{"turn:127.0.0.1:3478"}
			}
			if len(stunURLs) == 0 {
				stunURLs = []string{"stun:127.0.0.1:3478"}
			}
		}
	}
	rtcCreds := rtcapi.NewHandler(rtcapi.Config{
		Secret:   turnSecret,
		TurnURLs: turnURLs,
		StunURLs: stunURLs,
		TTL:      12 * time.Hour,
	}, rtcAuth, log.Default())

	// 发信通道：webhook（自有发码平台）> SMTP > Console 打印，见 README「发信配置」
	mail, mailName := mailer.New(mailer.Config{
		WebhookURL:   os.Getenv("LITETALK_MAILER_WEBHOOK"),
		WebhookToken: os.Getenv("LITETALK_MAILER_WEBHOOK_TOKEN"),
		SMTPHost:     os.Getenv("LITETALK_SMTP_HOST"),
		SMTPPort:     os.Getenv("LITETALK_SMTP_PORT"),
		SMTPUser:     os.Getenv("LITETALK_SMTP_USER"),
		SMTPPass:     os.Getenv("LITETALK_SMTP_PASS"),
		SMTPFrom:     os.Getenv("LITETALK_SMTP_FROM"),
	}, log.Default())
	log.Printf("发信通道: %s", mailName)

	// Auth 模式：full（默认，验证码流程）/ simple（免验证码，私域部署从简）。
	// 合法性已在 cfg.validate（步骤 2）把关。
	authMode := cfg.authMode
	if authMode == "simple" {
		log.Println("Auth 模式: simple（注册/找回免验证码，仅建议私域部署）")
	}

	// REST API（§5.4 全量）
	apiSrv := api.New(api.Config{
		Store:    st,
		JWT:      jwtSigner,
		Refresh:  refreshStore,
		Codes:    codes,
		Limits:   limits,
		Mailer:   mail,
		AuthMode: authMode,
		Log:      log.Default(),
		// 可信反代（Nginx 等）的 IP/CIDR，命中 RemoteAddr 才采用 X-Real-IP；
		// 不配置则只信 RemoteAddr。详见 deploy/env/litetalk.env.example。
		TrustedProxies: splitList(os.Getenv("LITETALK_TRUSTED_PROXIES")),
		// 多设备会话索引（v1.1.0 迁移 0004）：REST 中间件/WS 握手比对 sid。
		Session: sessIndex,
		// 信令 hub：登出/重置密码踢 WS（api.Hub 接口避免 signal→api 循环）。
		Hub: hub,
		// 在线状态读取（presence:{uid}，好友列表 user.online 填值）。
		Presence: kv,
		// 自托管形态自描述（GET /api/v1/config，#26）：SQLite/进程内单机部署 = true，
		// MySQL 全栈部署 = false。客户端据此显示自托管引导（邀请码等）。
		SelfHost: pickStoreKind() != "mysql",
		// 管理员邀请码是否未消费（#27 T4 填 bootstrap.Required 实现）。
		BootstrapRequired: func() bool { return bootstrap.Required(context.Background(), kv) },
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", healthzHandler)
	mux.Handle("POST /api/v1/rtc/credentials", rtcCreds)
	mux.Handle("/api/v1/", apiSrv.Handler())
	mux.HandleFunc("GET /ws", hub.ServeWS)

	// ---- bootstrap（§0.6 步骤 8）：首启生成管理员邀请码并打印（幂等）；#27 T4 填实现 ----
	bootstrap.Ensure(context.Background(), kv, st, cfg.jwtSecret, log.Default())

	// 捕获 SIGINT / SIGTERM，触发优雅关闭
	ctx, stop := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 起 HTTP（§0.6 步骤 9）：ACME TLS + :80 挑战，或纯 HTTP ----
	servers := startHTTP(cfg, mux, stop)
	log.Printf("litetalk server %s 监听 %s（SFU UDP %d / ICE-TCP %d，TURN %d）",
		version, cfg.addr, sfuInst.Port(), sfuInst.TCPPort(), turnPort)

	<-ctx.Done()
	log.Println("收到退出信号，开始优雅关闭")

	// 关闭序（§0.6 步骤 10）：hub 广播 server.shutdown → HTTP(S) 优雅关闭 → SFU/TURN/store。
	// defer 的 LIFO 会先关 TURN/SFU 再关存储，与正常退出顺序一致。
	hub.Shutdown("shutdown")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("优雅关闭失败: %v", err)
		}
	}
	log.Println("服务已停止")
}

// startHTTP 按配置启动监听（§0.5 三种形态）：
//   - ACME（--domain 且未 --behind-proxy）：cfg.addr TLS（TLS-ALPN-01 动态签发）
//   - :80 HTTP-01 挑战与 301；
//   - --behind-proxy：纯 HTTP，TLS 由前置反代终结，不占 80/443；
//   - 无 domain 无 behind-proxy：纯 HTTP（开发/自测）。
//
// 任一监听失败走正常退出路径（不 log.Fatalf），保证另一个 server 与 SFU 能优雅关闭。
func startHTTP(cfg *config, mux http.Handler, stop context.CancelFunc) []*http.Server {
	newServer := func(addr string, handler http.Handler) *http.Server {
		// 超时加固值（批次 3 #21）原样保留；WS 长连接在 Hijack 后不受这些超时约束。
		return &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
	}
	listen := func(name string, srv *http.Server, serve func() error, privPort bool) {
		go func() {
			if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				if privPort && isPermError(err) {
					log.Printf("%s 启动失败: %v（特权端口需 root 或 setcap cap_net_bind_service=+ep）", name, err)
				} else {
					log.Printf("%s 服务异常退出: %v", name, err)
				}
				stop() // 触发主 goroutine 走优雅关闭路径
			}
		}()
	}

	if cfg.acme {
		mgr, err := acme.New(cfg.domain, cfg.acmeCache)
		if err != nil {
			log.Printf("ACME 初始化失败: %v", err)
			stop()
			return nil
		}
		httpsSrv := newServer(cfg.addr, mux)
		httpsSrv.TLSConfig = mgr.TLSConfig()
		httpSrv := newServer(":80", mgr.HTTPHandler())
		log.Printf("ACME: 单域 %s 自动签发（TLS-ALPN-01 + HTTP-01），证书缓存 %s", cfg.domain, cfg.acmeCache)
		listen("HTTPS "+cfg.addr, httpsSrv, func() error { return httpsSrv.ListenAndServeTLS("", "") }, true)
		listen("HTTP :80", httpSrv, httpSrv.ListenAndServe, true)
		return []*http.Server{httpsSrv, httpSrv}
	}

	srv := newServer(cfg.addr, mux)
	if cfg.behindProxy {
		log.Println("TLS: 前置反代终结（--behind-proxy），本进程纯 HTTP，不占 80/443")
		// 不动 clientIP 语义：X-Real-IP 仍只在 RemoteAddr 命中 LITETALK_TRUSTED_PROXIES
		// 时采信（v1.0.2 修复），--behind-proxy 不自动放宽，防伪造头绕过限流。
		if len(splitList(os.Getenv("LITETALK_TRUSTED_PROXIES"))) == 0 {
			log.Println("提示: 未配置 LITETALK_TRUSTED_PROXIES，X-Real-IP 不会被采信，限流/审计记录的是反代 IP")
		}
	} else {
		log.Println("未启用 TLS；公网部署请用 --domain 或 --behind-proxy + 反代")
	}
	listen("HTTP "+cfg.addr, srv, srv.ListenAndServe, false)
	return []*http.Server{srv}
}

// isPermError 端口绑定权限不足（Linux 低段端口需 root / CAP_NET_BIND_SERVICE）。
func isPermError(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "access is denied")
}

// healthzHandler 健康检查：GET /api/v1/healthz
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version,
	})
}

// identityFromClaims 从 JWT Claims 构造信令身份。IssuedAt 透传供握手侧
// 旧版本令牌（无 sid）过渡期判定（v1.1.0 兼容窗口）。
func identityFromClaims(claims *auth.Claims) signal.Identity {
	id := signal.Identity{
		UserID:   strconv.FormatInt(claims.UserID, 10),
		Username: claims.Nickname,
		Sid:      claims.Sid,
		Dev:      claims.Dev,
	}
	if claims.IssuedAt != nil {
		id.IssuedAt = claims.IssuedAt.Unix()
	}
	return id
}

// devUserID 把 M2 开发令牌映射为用户 ID（M3 换 JWT 时同步替换）。
func devUserID(token string) (string, error) {
	id, err := signal.DevAuthenticator(token)
	return id.UserID, err
}

// ---- 环境变量小工具 ----

// weakSecrets 已公开的示例/默认密钥：与这些值相同即视为未配置（可伪造任意 uid 的令牌）。
var weakSecrets = map[string]bool{
	"litetalk-dev-secret-change-me": true, // auth.NewJWT 的 dev 回落值
	"CHANGE_ME":                     true, // server/deploy/env/litetalk.env.example 占位值
}

// validateJWTSecret 生产模式密钥准入（P0 S2）：空、不足 32 字节或等于公开示例值均拒绝。
// dev 模式不做限制——本地开发本来就允许空密钥回落到开发密钥。
func validateJWTSecret(secret string, dev bool) error {
	if dev {
		return nil
	}
	switch {
	case secret == "":
		return errors.New("未设置 LITETALK_JWT_SECRET")
	case weakSecrets[secret]:
		return errors.New("密钥仍是公开示例值（litetalk-dev-secret-change-me / CHANGE_ME），必须更换")
	case len(secret) < 32:
		return fmt.Errorf("密钥长度 %d 字节，不足 32 字节", len(secret))
	}
	return nil
}

// validateTURNSecret 显式配置的 TURN 密钥准入（#25）：内置 TURN 的 REST
// 凭证完全由该密钥派生，弱密钥等于任何人可自算凭证。未设置（空串）不在此
// 校验——由 resolveTURNSecret 走随机生成；dev 模式不做限制。
func validateTURNSecret(secret string, dev bool) error {
	if dev || secret == "" {
		return nil
	}
	if weakSecrets[secret] {
		return errors.New("LITETALK_TURN_SECRET 仍是公开示例值，必须更换为 ≥32 字节随机串（openssl rand -hex 32）")
	}
	if len(secret) < 32 {
		return fmt.Errorf("LITETALK_TURN_SECRET 长度 %d 字节，不足 32 字节（openssl rand -hex 32）", len(secret))
	}
	return nil
}

// resolveTURNSecret 决定 TURN 凭证密钥：显式值先过弱密钥校验；未显式配置且
// 内置 TURN 启用时随机生成（签发与校验同进程，互通无碍；重启后旧凭证失效，
// 客户端会重新取凭证）。generated 仅用于调用方打日志。
func resolveTURNSecret(explicit string, turnEnabled, dev bool) (secret string, generated bool, err error) {
	if err := validateTURNSecret(explicit, dev); err != nil {
		return "", false, err
	}
	if explicit != "" {
		return explicit, false, nil
	}
	if !turnEnabled {
		return "", false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("生成 TURN 凭证密钥失败: %w", err)
	}
	return hex.EncodeToString(buf), true, nil
}

// validateSFUIP 生产模式拒环回地址（#22）：SFU 对外通告 IP 为环回时客户端 ICE 候选不可达。
// dev 模式允许（本机联调）。
func validateSFUIP(ip string, dev bool) error {
	if dev {
		return nil
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("不是合法 IP: %q", ip)
	}
	if parsed.IsLoopback() {
		return fmt.Errorf("生产模式禁止环回地址（127.0.0.0/8、::1），实际 %q", ip)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s 不是合法整数: %q", key, v)
	}
	return n
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// pickStoreKind 决定存储后端（§0.3）：LITETALK_STORE 显式指定 > LITETALK_MYSQL_DSN
// 非空 → mysql（向后兼容，现有部署零改动）> 默认 sqlite（本批新默认）。
// 只解析不校验合法性——合法性在 main 的 switch 分支里按 dev/生产分别把关。
func pickStoreKind() string {
	if k := strings.ToLower(strings.TrimSpace(os.Getenv("LITETALK_STORE"))); k != "" {
		return k
	}
	if os.Getenv("LITETALK_MYSQL_DSN") != "" {
		return "mysql"
	}
	return "sqlite"
}

// resolveMigrationsMySQL 返回 MySQL 方言迁移 SQL 的文件系统：
// LITETALK_MIGRATIONS_DIR 非空时磁盘目录根覆盖（本地调试），否则 embed 的 migrations.FS 根。
// 根目录 *.sql 即 MySQL 方言；sqlite/ 子目录由 resolveMigrationsSQLite 负责，
// Migrate 的 fs.ReadDir + !e.IsDir() 天然跳过该目录。
func resolveMigrationsMySQL() fs.FS {
	if dir := os.Getenv("LITETALK_MIGRATIONS_DIR"); dir != "" {
		return os.DirFS(dir)
	}
	return migrations.FS
}

// resolveMigrationsSQLite 返回 SQLite 方言迁移 SQL（migrations/sqlite 子目录，同号同名）。
func resolveMigrationsSQLite() fs.FS {
	if dir := os.Getenv("LITETALK_MIGRATIONS_DIR"); dir != "" {
		return os.DirFS(filepath.Join(dir, "sqlite"))
	}
	sub, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		// embed 布局由 //go:embed *.sql sqlite/*.sql 编译期锁定，走到这里即打包坏了。
		log.Fatalf("migrations: 缺少 sqlite 子目录: %v", err)
	}
	return sub
}

// presenceDeps 把 KV（presence 键）与 Store（好友关系）拼成 signal.PresenceStore：
// 两者在 main 里是不同实例（Redis+MySQL 或双 Mem），需按方法分发到各自后端。
type presenceDeps struct {
	kv store.KV
	st store.Store
}

func (p presenceDeps) Get(ctx context.Context, key string) (string, error) {
	return p.kv.Get(ctx, key)
}

func (p presenceDeps) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return p.kv.Set(ctx, key, value, ttl)
}

func (p presenceDeps) Del(ctx context.Context, keys ...string) error {
	return p.kv.Del(ctx, keys...)
}

func (p presenceDeps) FriendshipsOfUser(ctx context.Context, userID int64) ([]store.Friendship, error) {
	return p.st.FriendshipsOfUser(ctx, userID)
}
