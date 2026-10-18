// 轻音 LiteTalk 服务端入口
// M3 阶段：healthz + WS 信令（GET /ws）+ 自研 Pion SFU + TURN 短期凭证 + 全量 REST API（账号/社交/组织）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	ossignal "os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/api"
	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/mailer"
	"github.com/OpenALC/LiteTalk/server/internal/rtcapi"
	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/migrations"
)

// version 由构建时 -X main.version= 注入，默认 dev
var version = "dev"

// 默认监听地址，可用环境变量 LITETALK_ADDR 覆盖
const defaultAddr = ":8077"

func main() {
	addr := envOr("LITETALK_ADDR", defaultAddr)

	// dev 模式判定：本地 go run / go build 未注入版本号时 version 为 "dev"；
	// CI 打包始终 -X main.version=<tag> 注入真实版本号，故发布产物一律视为生产模式，
	// 生产模式下内存降级、默认密钥、dev 令牌开关等开发便利项全部拒绝启动。
	dev := version == "dev"

	// ---- 持久化（MySQL + Redis，env 注入 DSN；不可用时内存降级） ----
	var st store.Store
	var kv store.KV
	if dsn := os.Getenv("LITETALK_MYSQL_DSN"); dsn != "" {
		my, err := store.OpenMySQL(dsn)
		if err != nil {
			log.Fatalf("MySQL 连接失败: %v", err)
		}
		defer my.Close()
		migFS := resolveMigrations()
		if err := my.Migrate(context.Background(), migFS); err != nil {
			log.Fatalf("数据库迁移失败: %v", err)
		}
		st = my
		log.Println("持久化: MySQL")
	} else if dev {
		st = store.NewMem()
		log.Println("持久化: 内存（设置 LITETALK_MYSQL_DSN 启用 MySQL）")
	} else {
		// 生产模式拒绝内存降级：重启后用户/服务器/消息全部丢失，属静默数据事故。
		log.Fatal("未配置 LITETALK_MYSQL_DSN：生产模式必须以 MySQL 启动（内存存储仅限 dev 模式本地开发）")
	}
	if rdb, err := store.OpenRedis(os.Getenv("LITETALK_REDIS_ADDR")); err == nil {
		defer rdb.Close()
		kv = rdb
		log.Println("KV: Redis")
	} else if dev {
		kv = store.NewMem()
		log.Println("KV: 内存（LITETALK_REDIS_ADDR 不可达，降级内存）")
	} else {
		// 生产模式拒绝内存 KV：刷新令牌/验证码/限流重启即失，会话全灭。
		log.Fatalf("Redis 连接失败（%v）：生产模式必须配置可用的 LITETALK_REDIS_ADDR，拒绝降级内存 KV", err)
	}

	// ---- 账号安全 ----
	jwtSecret := os.Getenv("LITETALK_JWT_SECRET")
	if err := validateJWTSecret(jwtSecret, dev); err != nil {
		log.Fatalf("LITETALK_JWT_SECRET 不合法：%v（请设为至少 32 字节的随机串，如 openssl rand -hex 32）", err)
	}
	// SFU IP 校验：生产拒环回（#22）。
	sfuIP := envOr("LITETALK_SFU_IP", "127.0.0.1")
	if err := validateSFUIP(sfuIP, dev); err != nil {
		log.Fatalf("LITETALK_SFU_IP 不合法：%v", err)
	}
	jwtSigner := auth.NewJWT(jwtSecret)
	refreshStore := auth.NewRefreshStore(kv)
	codes := auth.NewEmailCodes(kv)
	limits := auth.NewRateLimit(kv)
	sessIndex := auth.NewSessionIndex(st, kv)

	// SFU：ICE-Lite + 单 UDP mux（§5.2）
	sfuInst, err := sfu.New(sfu.Config{
		UDPPort:    envInt("LITETALK_SFU_UDP", 7845),
		NAT1To1IP:  sfuIP,
		MaxMembers: envInt("LITETALK_SFU_MAX", 10),
		Logger:     log.Default(),
	})
	if err != nil {
		log.Fatalf("SFU 初始化失败: %v", err)
	}
	defer sfuInst.Close()

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
		Store:    st,
		Limits:   limits,
		Presence: presenceDeps{kv: kv, st: st},
		Dev:      dev,
	})

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
	rtcCreds := rtcapi.NewHandler(rtcapi.Config{
		Secret:   os.Getenv("LITETALK_TURN_SECRET"),
		TurnURLs: splitList(envOr("LITETALK_TURN_URLS", "turn:127.0.0.1:3478")),
		StunURLs: splitList(envOr("LITETALK_STUN_URLS", "stun:127.0.0.1:3478")),
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

	// Auth 模式：full（默认，验证码流程）/ simple（免验证码，私域部署从简）
	authMode := envOr("LITETALK_AUTH_MODE", "full")
	if authMode != "full" && authMode != "simple" {
		log.Fatalf("LITETALK_AUTH_MODE 只支持 full / simple，实际为 %q", authMode)
	}
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
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", healthzHandler)
	mux.Handle("POST /api/v1/rtc/credentials", rtcCreds)
	mux.Handle("/api/v1/", apiSrv.Handler())
	mux.HandleFunc("GET /ws", hub.ServeWS)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 捕获 SIGINT / SIGTERM，触发优雅关闭
	ctx, stop := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("litetalk server %s 监听 %s（SFU UDP %d）", version, addr, sfuInst.Port())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP 服务异常退出: %v", err)
			stop() // 触发主 goroutine 走优雅关闭路径
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，开始优雅关闭")

	// 关闭序：hub 广播 server.shutdown → HTTP 优雅关闭 → SFU 关闭。
	// defer 的 LIFO 会先关 SFU 再关 MySQL，与正常退出顺序一致。
	hub.Shutdown("shutdown")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅关闭失败: %v", err)
	}
	log.Println("服务已停止")
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

// resolveMigrations 返回迁移 SQL 的文件系统：
// LITETALK_MIGRATIONS_DIR 非空时用磁盘目录覆盖（本地调试）；否则用 embed 的 migrations.FS。
func resolveMigrations() fs.FS {
	if dir := os.Getenv("LITETALK_MIGRATIONS_DIR"); dir != "" {
		return os.DirFS(dir)
	}
	return migrations.FS
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
