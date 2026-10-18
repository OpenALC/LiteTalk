// Package acme 内置 ACME 自动签发（#26）：--domain 单域名证书申请与续期。
// 挑战双栈（§10 裁定 4）：443 TLS-ALPN-01 + 80 HTTP-01；80 非挑战路径 301 跳 https。
// HostPolicy 必须限定单域名——不加白名单会让任意 SNI 申请证书，被 CA 限流甚至封号。
package acme

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"

	"golang.org/x/crypto/acme/autocert"
)

// Manager autocert 的薄封装：单域签发 + 目录缓存。
type Manager struct {
	m      *autocert.Manager
	domain string
}

// New 构造签发管理器。cacheDir 为证书缓存目录（默认 $LITETALK_DATA_DIR/autocert），
// 预先以 0700 创建——私钥不能落成全局可读（autocert.DirCache 写入的文件本身是 0600）。
func New(domain, cacheDir string) (*Manager, error) {
	if domain == "" {
		return nil, fmt.Errorf("acme: 域名为空")
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("acme: 证书缓存目录为空")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("acme: 建证书缓存目录: %w", err)
	}
	return &Manager{
		m: &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: hostPolicy(domain),
			Cache:      autocert.DirCache(cacheDir),
		},
		domain: domain,
	}, nil
}

// hostPolicy 单域名白名单：先剥离端口再精确匹配。
// 不直接用 autocert.HostWhitelist——HTTPHandler 传给 HostPolicy 的是原始 r.Host，
// 可能带 :80 端口，白名单是精确 map 查找会误拒 HTTP-01 挑战（GetCertificate 路径
// 的 SNI autocert 自己已归一化，剥离端口对它无副作用）。
func hostPolicy(domain string) autocert.HostPolicy {
	base := autocert.HostWhitelist(domain)
	return func(ctx context.Context, host string) error {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return base(ctx, host)
	}
}

// TLSConfig 主 HTTPS server 的 tls.Config：GetCertificate 动态签发 + ALPN 挑战。
// 不要再塞静态 Certificates，会覆盖动态签发。
func (a *Manager) TLSConfig() *tls.Config { return a.m.TLSConfig() }

// HTTPHandler 80 端口处理器：/.well-known/acme-challenge/ 走 HTTP-01，
// 其余路径 301 跳 https（autocert 默认 fallback 是 302，契约要 301）。
func (a *Manager) HTTPHandler() http.Handler {
	return a.m.HTTPHandler(http.HandlerFunc(redirectHTTPS))
}

// redirectHTTPS 非挑战路径统一 301 到 https。
func redirectHTTPS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Use HTTPS", http.StatusBadRequest)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
}
