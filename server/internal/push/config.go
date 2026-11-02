package push

import (
	"log"
	"os"
	"strings"
)

// Config 推送配置（环境变量 > 配置文件 > 默认值；本包只收已解析的值，
// env 读取见 FromEnv）。Provider 为空 = 功能关闭，New 返回 Noop。
type Config struct {
	// Provider noop/空（关闭）/ fcm / apns / ntfy。未知值按关闭处理并告警。
	Provider string
	// FCMCredentials 服务账号 JSON（内联）或文件路径。
	FCMCredentials string
	// APNs 凭据：.p8 私钥（内联 PEM 或路径）、Key ID、Team ID、bundle id。
	APNSKey     string
	APNSKeyID   string
	APNSTeamID  string
	APNSTopic   string
	APNSSandbox bool
	// NtfyURL ntfy 服务器根 URL（如 https://ntfy.sh）。
	NtfyURL string
	Log     *log.Logger
}

// 环境变量名（server/deploy/env/litetalk.env.example 同步维护）。
const (
	envProvider    = "LITETALK_PUSH_PROVIDER"
	envFCMCred     = "LITETALK_PUSH_FCM_CREDENTIALS"
	envAPNSKey     = "LITETALK_PUSH_APNS_KEY"
	envAPNSKeyID   = "LITETALK_PUSH_APNS_KEY_ID"
	envAPNSTeam    = "LITETALK_PUSH_APNS_TEAM_ID"
	envAPNSTopic   = "LITETALK_PUSH_APNS_TOPIC"
	envAPNSSandbox = "LITETALK_PUSH_APNS_SANDBOX"
	envNtfyURL     = "LITETALK_PUSH_NTFY_URL"
)

// FromEnv 读环境变量组装 Config。provider 显式写 noop 等价于留空（关闭）。
func FromEnv() Config {
	return Config{
		Provider:       os.Getenv(envProvider),
		FCMCredentials: envFileOrInline(envFCMCred),
		APNSKey:        envFileOrInline(envAPNSKey),
		APNSKeyID:      os.Getenv(envAPNSKeyID),
		APNSTeamID:     os.Getenv(envAPNSTeam),
		APNSTopic:      os.Getenv(envAPNSTopic),
		APNSSandbox:    os.Getenv(envAPNSSandbox) == "1",
		NtfyURL:        strings.TrimRight(os.Getenv(envNtfyURL), "/"),
		Log:            log.Default(),
	}
}

// envFileOrInline 凭据值可以是内联内容（JSON/PEM），也可以是文件路径：
// 以 "{"/"-----BEGIN" 开头视为内联，否则按路径读取。读取失败保留原值，
// 由实现方在解析时报错（启动不因推送凭据缺失而失败——推送是旁路能力）。
func envFileOrInline(key string) string {
	v := os.Getenv(key)
	if v == "" || strings.HasPrefix(v, "{") || strings.Contains(v, "-----BEGIN") {
		return v
	}
	data, err := os.ReadFile(v)
	if err != nil {
		return v // 交给实现方解析时给出可读错误
	}
	return string(data)
}

// New 按 Provider 选实现（mailer.New 同构）：返回实现与名称。
// 关闭、未知值或该通道配置不全 → Noop（零 I/O），配置不全时打告警日志。
func New(cfg Config) (Pusher, string) {
	lg := safeLog(cfg.Log)
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "":
		return Noop{}, "noop"
	case "noop":
		return Noop{}, "noop"
	case "ntfy":
		if cfg.NtfyURL == "" {
			lg.Println("[push] LITETALK_PUSH_PROVIDER=ntfy 但未配置 LITETALK_PUSH_NTFY_URL，推送保持关闭")
			return Noop{}, "noop"
		}
		return &Ntfy{URL: cfg.NtfyURL, Log: cfg.Log}, "ntfy"
	case "fcm":
		if cfg.FCMCredentials == "" {
			lg.Println("[push] LITETALK_PUSH_PROVIDER=fcm 但未配置 LITETALK_PUSH_FCM_CREDENTIALS，推送保持关闭")
			return Noop{}, "noop"
		}
		return &FCM{Credentials: cfg.FCMCredentials, Log: cfg.Log}, "fcm"
	case "apns":
		if cfg.APNSKey == "" || cfg.APNSKeyID == "" || cfg.APNSTeamID == "" || cfg.APNSTopic == "" {
			lg.Println("[push] LITETALK_PUSH_PROVIDER=apns 需要同时配置 APNS_KEY / APNS_KEY_ID / APNS_TEAM_ID / APNS_TOPIC，推送保持关闭")
			return Noop{}, "noop"
		}
		return &APNs{
			Key: cfg.APNSKey, KeyID: cfg.APNSKeyID, TeamID: cfg.APNSTeamID,
			Topic: cfg.APNSTopic, Sandbox: cfg.APNSSandbox, Log: cfg.Log,
		}, "apns"
	default:
		lg.Printf("[push] 未知 LITETALK_PUSH_PROVIDER=%q，推送保持关闭", cfg.Provider)
		return Noop{}, "noop"
	}
}
