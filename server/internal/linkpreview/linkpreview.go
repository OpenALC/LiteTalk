// Package linkpreview 抓取外部网页的链接预览（#42 B4）。
//
// 服务端替客户端访问任意用户提供的 URL，是典型的 SSRF 面：只允许 http/https，
// 拒绝 userinfo 与私网/回环/链路本地/CGNAT/云元数据地址；DNS 在真正拨号时解析
// 并逐 IP 校验（防 DNS 重绑定），重定向逐跳复用同一套校验。响应限制超时与字节数，
// 只解析 HTML 的 OpenGraph / <title> / description / twitter:image。
//
// 不提供「允许私网」开关：需要预览内网地址的部署不在支持范围内。
package linkpreview

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrBlocked 目标地址被安全策略拒绝（非法 scheme/userinfo、私网/保留地址、
	// 重定向逃逸等）。API 层映射 400 bad_url，且不回传具体解析细节。
	ErrBlocked = errors.New("linkpreview: 目标地址不被允许")
	// ErrNoPreview 抓取成功但拿不到可展示的预览（非 200/非 HTML/无元数据/
	// 超时/超限/网络失败）。API 层映射 404 no_preview。
	ErrNoPreview = errors.New("linkpreview: 没有可用的预览")
)

// Preview 一条链接预览。所有文本字段已折叠空白、去除控制字符并按 rune 截断；
// URL 为跟随重定向后的最终页面地址。
type Preview struct {
	URL         string
	Title       string
	Description string
	SiteName    string
	ImageURL    string
}

// Config 抓取参数。零值回落到默认值（5s / 512 KiB）。
type Config struct {
	Timeout  time.Duration
	MaxBytes int64
}

// 默认值与边界：与 server/cmd/server 的环境变量夹取区间保持一致。
const (
	DefaultTimeout  = 5 * time.Second
	DefaultMaxBytes = 512 << 10
	// MaxURLLength URL 原文字节上限（契约 1–2048 字符，字节口径更严且足够）。
	MaxURLLength = 2048
)

// maxRedirects 最多跟随的跳转次数（第 4 次 Location 直接拒绝）。
const maxRedirects = 3

// blockedNets 私网/保留/不可路由网段。IPv4-mapped 在 isBlockedIP 里先归一为
// IPv4 再匹配，因此这里不需要重复列 ::ffff:0:0/96。
var blockedNets = mustCIDRs(
	"0.0.0.0/8",       // 本网络（含 0.0.0.0）
	"10.0.0.0/8",      // 私网
	"100.64.0.0/10",   // CGNAT
	"127.0.0.0/8",     // 回环
	"169.254.0.0/16",  // 链路本地（含 169.254.169.254 云元数据）
	"172.16.0.0/12",   // 私网
	"192.0.0.0/24",    // IETF 协议分配
	"192.0.2.0/24",    // 文档
	"192.168.0.0/16",  // 私网
	"198.18.0.0/15",   // 基准测试
	"198.51.100.0/24", // 文档
	"203.0.113.0/24",  // 文档
	"224.0.0.0/4",     // 组播
	"240.0.0.0/4",     // 保留（含 255.255.255.255 广播）
	"::/128",          // 未指定
	"::1/128",         // IPv6 回环
	"::/96",           // IPv4-compatible（已废弃；攻击者可借网关折算到私网 IPv4）
	"64:ff9b::/96",    // NAT64 well-known prefix（内嵌 IPv4，可折算）
	"64:ff9b:1::/48",  // NAT64 local-use
	"2002::/16",       // 6to4（内嵌 IPv4，可折算）
	"fc00::/7",        // IPv6 ULA
	"fe80::/10",       // IPv6 链路本地
	"ff00::/8",        // IPv6 组播
)

func mustCIDRs(entries ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			panic("linkpreview: 非法内置 CIDR " + e + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// isBlockedIP 判定 IP 是否禁止访问。nil/无法解析按禁止处理（fail-closed）。
// IPv4-mapped（::ffff:a.b.c.d）先 To4 归一，避免用映射地址绕过 IPv4 黑名单。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if ip.To16() == nil {
		return true
	}
	// 标准库的语义判定兜底（回环/未指定/组播/链路本地），CIDR 表覆盖其余段。
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validateURL 校验并解析原始 URL：只允许 http/https、拒绝 userinfo、host 必填、
// 长度 ≤ MaxURLLength、端口合法、字面量 IP 不在黑名单、host 不是 localhost。
// 域名是否指向私网由拨号时的 safeDialContext 判定（防 DNS 重绑定）。
func validateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxURLLength {
		return nil, ErrBlocked
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrBlocked
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, ErrBlocked
	}
	if u.User != nil {
		return nil, ErrBlocked
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, ErrBlocked
	}
	// 末尾点等价于同一主机名，先剥掉再判断 localhost 与 IP。
	host = strings.TrimSuffix(host, ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, ErrBlocked
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, ErrBlocked
		}
	}
	// IPv6 作用域（fe80::1%eth0）：剥掉 zone 后再按 IP 判定。
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return nil, ErrBlocked
	}
	return u, nil
}
