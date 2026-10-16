// 轻音 LiteTalk 服务端入口
// M3 阶段：healthz + WS 信令（GET /ws）+ 自研 Pion SFU + TURN 短期凭证 + 全量 REST API（账号/社交/组织）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
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
		migDir := envOr("LITETALK_MIGRATIONS_DIR", "migrations")
		if err := my.Migrate(context.Background(), migDir); err != nil {
			log.Fatalf("数据库迁移失败: %v", err)
		}
		st = my
		log.Println("持久化: MySQL")
	} else {
		st = store.NewMem()
		log.Println("持久化: 内存（设置 LITETALK_MYSQL_DSN 启用 MySQL）")
	}
	if rdb, err := store.OpenRedis(os.Getenv("LITETALK_REDIS_ADDR")); err == nil {
		defer rdb.Close()
		kv = rdb
		log.Println("KV: Redis")
	} else {
		kv = store.NewMem()
		log.Println("KV: 内存（LITETALK_REDIS_ADDR 不可达，降级内存）")
	}

	// ---- 账号安全 ----
	jwtSigner := auth.NewJWT(os.Getenv("LITETALK_JWT_SECRET"))
	refreshStore := auth.NewRefreshStore(kv)
	codes := auth.NewEmailCodes(kv)
	limits := auth.NewRateLimit(kv)

	// SFU：ICE-Lite + 单 UDP mux（§5.2）
	sfuInst, err := sfu.New(sfu.Config{
		UDPPort:    envInt("LITETALK_SFU_UDP", 7845),
		NAT1To1IP:  envOr("LITETALK_SFU_IP", "127.0.0.1"),
		MaxMembers: envInt("LITETALK_SFU_MAX", 10),
		Logger:     log.Default(),
	})
	if err != nil {
		log.Fatalf("SFU 初始化失败: %v", err)
	}
	defer sfuInst.Close()

	// WS 信令 hub：默认 JWT 鉴权；LITETALK_DEV_AUTH=1 时 dev- 前缀令牌兜底（JWT 优先）。
	var authenticator signal.Authenticator
	if os.Getenv("LITETALK_DEV_AUTH") == "1" {
		authenticator = func(token string) (string, string, error) {
			if !strings.HasPrefix(token, "dev-") {
				if claims, err := jwtSigner.ParseAccess(token); err == nil {
					return strconv.FormatInt(claims.UserID, 10), claims.Nickname, nil
				}
			}
			return signal.DevAuthenticator(token)
		}
		log.Println("WS 鉴权: JWT + dev-token 兜底（开发模式）")
	} else {
		authenticator = func(token string) (string, string, error) {
			claims, err := jwtSigner.ParseAccess(token)
			if err != nil {
				return "", "", signal.ErrUnauthorized
			}
			return strconv.FormatInt(claims.UserID, 10), claims.Nickname, nil
		}
		log.Println("WS 鉴权: JWT")
	}
	hub := signal.NewHub(sfuInst, authenticator, log.Default(), 0, signal.ChatDeps{
		Store:  st,
		Limits: limits,
		Dev:    dev,
	})

	// TURN 短期凭证签发（coturn use-auth-secret，§5.4）
	var rtcAuth rtcapi.UserFromToken
	if os.Getenv("LITETALK_DEV_AUTH") == "1" {
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
	}

	// 捕获 SIGINT / SIGTERM，触发优雅关闭
	ctx, stop := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("litetalk server %s 监听 %s（SFU UDP %d）", version, addr, sfuInst.Port())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，开始优雅关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("优雅关闭失败: %v", err)
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

// devUserID 把 M2 开发令牌映射为用户 ID（M3 换 JWT 时同步替换）。
func devUserID(token string) (string, error) {
	id, _, err := signal.DevAuthenticator(token)
	return id, err
}

// ---- 环境变量小工具 ----

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
