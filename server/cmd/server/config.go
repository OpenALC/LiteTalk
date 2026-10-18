// 启动配置解析（#26）：stdlib flag，优先级 flag > env > 默认值（§0.6）。
// 本批不引入配置文件；LITETALK_* 是运维配置面，flag 供命令行一键自托管。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// config 启动配置。只收 T3（#26）负责的面：TLS/域名/数据目录/主流程校验输入；
// 存储选型（pickStoreKind）、SFU/TURN 端口等环境变量仍在各自区段读取，不搬动。
type config struct {
	domain           string // -domain / LITETALK_DOMAIN：ACME 单域签发 + TURN URL host
	behindProxy      bool   // -behind-proxy / LITETALK_BEHIND_PROXY：前置反代，关闭内置 TLS/ACME
	dataDir          string // -data / LITETALK_DATA_DIR：SQLite、证书缓存、bootstrap 状态
	acmeCache        string // LITETALK_ACME_CACHE：证书缓存目录，默认 $dataDir/autocert
	addr             string // 监听地址；ACME 且 LITETALK_ADDR 未显式配置时自动切 :443
	addrExplicit     bool   // LITETALK_ADDR 显式配置过则绝不自动改端口
	acme             bool   // 启用内置 ACME（domain 非空且未 behind-proxy）
	printAdminInvite bool   // -print-admin-invite：打印管理员邀请码后退出（#27 T4 填实现）

	// 校验输入（启动序步骤 2 集中把关；本批无对应 flag，仅 env）
	jwtSecret string
	sfuIP     string
	authMode  string
}

// loadConfig 解析 flag 与 env。output 为 flag 用法/错误输出（main 传 os.Stderr，测试传 io.Discard）。
// flag 包同时接受 -domain 与 --domain。
func loadConfig(args []string, output io.Writer) (*config, error) {
	fs := flag.NewFlagSet("litetalk-server", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "用法: litetalk-server [选项]")
		fmt.Fprintln(output, "一键自托管: litetalk-server --domain voice.example.com")
		fs.PrintDefaults()
	}

	domain := fs.String("domain", os.Getenv("LITETALK_DOMAIN"),
		"启用 ACME 自动签发的域名（同时用作 TURN/STUN URL 的 host）")
	behindProxy := fs.Bool("behind-proxy", envBool("LITETALK_BEHIND_PROXY"),
		"TLS 由前置反代终结：不启 ACME/内置 TLS，保持纯 HTTP")
	dataDir := fs.String("data", envOr("LITETALK_DATA_DIR", "./data"),
		"数据目录（SQLite 库、证书缓存、bootstrap 状态）")
	printAdminInvite := fs.Bool("print-admin-invite", false,
		"重新打印管理员邀请码后退出")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("无法识别的参数: %s", strings.Join(fs.Args(), " "))
	}

	cfg := &config{
		domain:           strings.TrimSpace(*domain),
		behindProxy:      *behindProxy,
		dataDir:          *dataDir,
		printAdminInvite: *printAdminInvite,
		jwtSecret:        os.Getenv("LITETALK_JWT_SECRET"),
		sfuIP:            envOr("LITETALK_SFU_IP", "127.0.0.1"),
		authMode:         envOr("LITETALK_AUTH_MODE", "full"),
	}
	if cfg.dataDir == "" {
		cfg.dataDir = "./data"
	}
	if v := os.Getenv("LITETALK_ACME_CACHE"); v != "" {
		cfg.acmeCache = v
	} else {
		cfg.acmeCache = filepath.Join(cfg.dataDir, "autocert")
	}
	cfg.addr = envOr("LITETALK_ADDR", defaultAddr)
	cfg.addrExplicit = os.Getenv("LITETALK_ADDR") != ""
	// 触发条件（§0.5）：--domain 非空 且 未设 --behind-proxy。
	cfg.acme = cfg.domain != "" && !cfg.behindProxy
	// ACME 模式下默认端口切 443；显式设了 LITETALK_ADDR 就不许再自动改（环境变量 > 默认值）。
	if cfg.acme && !cfg.addrExplicit && cfg.addr == defaultAddr {
		cfg.addr = ":443"
	}
	return cfg, nil
}

// envBool 环境变量布尔：1 / true 视为开（§0.6 LITETALK_BEHIND_PROXY 语义），其余关。
func envBool(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true")
}

// validate 启动序步骤 2 集中校验（§0.6）：JWT secret、SFU IP 拒环回、auth mode、域名形态。
// store 选型合法性由持久化区段的 pickStoreKind switch 把关（打开任何连接之前同样会拒）。
// 生产三件套里的 KV 守卫在持久化区段（#24 已放宽：未配 Redis 走进程内 MemKV + 明示日志）。
func (c *config) validate(dev bool) error {
	if err := validateJWTSecret(c.jwtSecret, dev); err != nil {
		return fmt.Errorf("LITETALK_JWT_SECRET 不合法: %w（请设为至少 32 字节的随机串，如 openssl rand -hex 32）", err)
	}
	if err := validateSFUIP(c.sfuIP, dev); err != nil {
		return fmt.Errorf("LITETALK_SFU_IP 不合法: %w", err)
	}
	if c.authMode != "full" && c.authMode != "simple" {
		return fmt.Errorf("LITETALK_AUTH_MODE 只支持 full / simple，实际为 %q", c.authMode)
	}
	if c.domain != "" {
		// 防止把 URL 或带路径/端口的值当域名传入，签发期才失败更难排查。
		if strings.ContainsAny(c.domain, "/:\\ ") {
			return fmt.Errorf("LITETALK_DOMAIN 只应是域名本身（不含协议/路径/端口），实际 %q", c.domain)
		}
	}
	return nil
}
