// Package updater 服务端 Release 自动更新器（#97）。
// webhook → HMAC 校验 → 下载 + SHA256 → 原子替换 → systemd 重启 → healthz 断言 → 失败回滚。
// 不导入 server/internal 业务包：独立小二进制、低攻击面。
// 监听明文 HTTP（默认 :8088）：公网仅暴露 webhook，/status 仅 loopback；
// 443 与证书签发归主服务，本进程不碰 TLS。
package updater

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 默认值与生产部署形态对齐（/www/wwwroot/LiteTalk + www 用户 + litetalk.service）。
const (
	DefaultListen       = ":8088"
	DefaultRepo         = "OpenALC/LiteTalk"
	DefaultInstallDir   = "/www/wwwroot/LiteTalk"
	DefaultService      = "litetalk.service"
	DefaultHealthURL    = "http://127.0.0.1:8077/api/v1/healthz"
	DefaultStateDir     = "/var/lib/litetalk-updater"
	DefaultSecretFile   = "/etc/litetalk-updater.webhook.secret"
	DefaultGitHubAPI    = "https://api.github.com"
	DefaultHealthTimout = 60 * time.Second

	// MaxBodyBytes webhook 请求体上限：payload 只有几百字节，1MiB 足够且防放大。
	MaxBodyBytes = 1 << 20
	// MinSecretLen webhook secret 最小长度：与 JWT/TURN 同口径（32 字节随机值）。
	MinSecretLen = 32
	// MinFreeBytes 更新前要求的磁盘可用空间：覆盖新旧二进制 + 备份 + 下载临时文件。
	MinFreeBytes = 200 << 20
	// MaxExtractBytes 单个解包条目大小上限（正常二进制远小于此，防解包炸弹）。
	MaxExtractBytes = 256 << 20
	// BackupKeep 备份保留份数（litetalk-server.bak-* 只留最近 3 份）。
	BackupKeep = 3
	// DeliveryKeep 去重记录的 delivery ID 保留条数（环形淘汰，防状态文件膨胀）。
	DeliveryKeep = 32
)

// TagRe 发布 tag 白名单：v<大>.<功能>.<修复>[-(alpha|beta|rc)]，与 CI 的 check-tag 一致。
var TagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc))?$`)

// repoRe 仓库白名单形态 owner/name，启动前校验，防配置注入。
var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// serviceRe systemd 单元名形态；作为 exec 参数前仍校验，防配置注入。
var serviceRe = regexp.MustCompile(`^[A-Za-z0-9@_.-]+$`)

// Config 更新器配置。优先级：命令行 flag > 环境变量（LITETALK_UPDATER_*）> 默认值。
type Config struct {
	// Enabled 为 false 时 webhook 只校验/记录并忽略（202），不下载不替换不重启；
	// --once 手工升级不受此项影响（运维显式操作）。
	Enabled bool
	// AllowPrerelease 默认 false：只接受正式版 tag；显式开启才接受 -alpha/-beta/-rc。
	AllowPrerelease bool

	Listen        string
	SecretFile    string
	Secret        []byte // 测试/嵌入场景直接注入；为空时从 SecretFile 读
	Repo          string
	AssetName     string // 空则按 GOARCH 生成 litetalk-server-linux-<arch>.tar.gz
	InstallDir    string
	ServiceName   string
	HealthURL     string
	HealthTimeout time.Duration
	StateDir      string
	GitHubAPI     string

	// 运行期开关（--once 用）。
	DryRun    bool
	NoRestart bool
}

// DefaultConfig 默认配置。Enabled=1、AllowPrerelease=0（默认只发正式版）。
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		AllowPrerelease: false,
		Listen:          DefaultListen,
		SecretFile:      DefaultSecretFile,
		Repo:            DefaultRepo,
		InstallDir:      DefaultInstallDir,
		ServiceName:     DefaultService,
		HealthURL:       DefaultHealthURL,
		HealthTimeout:   DefaultHealthTimout,
		StateDir:        DefaultStateDir,
		GitHubAPI:       DefaultGitHubAPI,
	}
}

// LoadEnv 读取 LITETALK_UPDATER_* 覆盖当前值。取值非法直接报错（fail-closed），不静默回落。
// 旧版 TLS 相关变量（域名/明文开关/证书缓存）已废弃：升级时留在环境里不报错，但不再读取。
func (c *Config) LoadEnv(getenv func(string) string) error {
	str := func(k string, dst *string) {
		if v := getenv(k); v != "" {
			*dst = v
		}
	}
	var err error
	if v := getenv("LITETALK_UPDATER_ENABLED"); v != "" {
		if c.Enabled, err = parseBool(v); err != nil {
			return fmt.Errorf("LITETALK_UPDATER_ENABLED: %w", err)
		}
	}
	if v := getenv("LITETALK_UPDATER_ALLOW_PRERELEASE"); v != "" {
		if c.AllowPrerelease, err = parseBool(v); err != nil {
			return fmt.Errorf("LITETALK_UPDATER_ALLOW_PRERELEASE: %w", err)
		}
	}
	str("LITETALK_UPDATER_LISTEN", &c.Listen)
	str("LITETALK_UPDATER_SECRET_FILE", &c.SecretFile)
	str("LITETALK_UPDATER_REPO", &c.Repo)
	str("LITETALK_UPDATER_ASSET", &c.AssetName)
	str("LITETALK_UPDATER_INSTALL_DIR", &c.InstallDir)
	str("LITETALK_UPDATER_SERVICE", &c.ServiceName)
	str("LITETALK_UPDATER_HEALTH_URL", &c.HealthURL)
	str("LITETALK_UPDATER_STATE_DIR", &c.StateDir)
	str("LITETALK_UPDATER_GITHUB_API", &c.GitHubAPI)
	if v := getenv("LITETALK_UPDATER_HEALTH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("LITETALK_UPDATER_HEALTH_TIMEOUT: %w", err)
		}
		c.HealthTimeout = d
	}
	return nil
}

// Validate 校验配置形态。once 模式不需要 secret/监听等服务端项。
func (c Config) Validate(once bool) error {
	if !repoRe.MatchString(c.Repo) {
		return fmt.Errorf("仓库白名单形态非法（应为 owner/name）: %q", c.Repo)
	}
	if !serviceRe.MatchString(c.ServiceName) {
		return fmt.Errorf("服务名单元形态非法: %q", c.ServiceName)
	}
	if !isAbsLinux(c.InstallDir) {
		return fmt.Errorf("安装目录必须是 Linux 绝对路径: %q", c.InstallDir)
	}
	if !isAbsLinux(c.StateDir) {
		return fmt.Errorf("状态目录必须是 Linux 绝对路径: %q", c.StateDir)
	}
	u, err := url.Parse(c.HealthURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("healthz URL 非法: %q", c.HealthURL)
	}
	if c.HealthTimeout <= 0 {
		return fmt.Errorf("healthz 超时必须为正: %v", c.HealthTimeout)
	}
	if once {
		return nil
	}
	_, portStr, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("监听地址非法（应形如 host:port，如 :8088）: %q: %w", c.Listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("监听端口不是数字: %q", portStr)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("监听端口超出 1..65535: %d", port)
	}
	if port < 1024 {
		return fmt.Errorf("拒绝监听特权端口 %d：updater 已改为 8088 明文、443 归主服务 ACME，请删除旧 TLS 环境变量并使用 :8088", port)
	}
	return nil
}

// AssetFileName 本次更新要下载的资产名（含架构）。
func (c Config) AssetFileName(goarch string) string {
	if c.AssetName != "" {
		return c.AssetName
	}
	return fmt.Sprintf("litetalk-server-linux-%s.tar.gz", goarch)
}

// parseBool 只接受 1/0/true/false/yes/no（大小写不敏感），其余报错。
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("无法解析布尔值 %q", s)
}

// isAbsLinux 部署目标是 Linux，按 Unix 绝对路径校验（filepath.IsAbs 在 Windows 开发机上会误拒 /www/...）。
// 同时拒 ".."：路径拼接里不允许相对跳转。
func isAbsLinux(p string) bool {
	return len(p) > 1 && p[0] == '/' && !strings.Contains(p, "..")
}
