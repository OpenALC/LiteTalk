package linkpreview

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// userAgent 抓取标识：带上项目地址，方便站点管理员识别与联系。
const userAgent = "LiteTalkBot/0.1 (+https://github.com/OpenALC/LiteTalk)"

// Fetcher 链接预览抓取器。client 的 RoundTripper 在测试中可替换为返回固定
// HTML 的桩；lookupIP/dialContext 可注入，生产由 New 填默认实现。
type Fetcher struct {
	timeout  time.Duration
	maxBytes int64
	client   *http.Client
	// lookupIP 与实际拨号分离：safeDialContext 先解析、校验全部 IP，再用
	// 校验过的 IP 拨号，杜绝「校验用一次解析、拨号再解析」的 DNS 重绑定窗口。
	lookupIP    func(ctx context.Context, host string) ([]net.IPAddr, error)
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New 构造抓取器。cfg 零值回落默认（5s / 512 KiB）。
func New(cfg Config) *Fetcher {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	f := &Fetcher{
		timeout:  cfg.Timeout,
		maxBytes: cfg.MaxBytes,
	}
	f.lookupIP = net.DefaultResolver.LookupIPAddr
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	f.dialContext = dialer.DialContext
	f.client = &http.Client{
		Timeout:       cfg.Timeout,
		CheckRedirect: f.checkRedirect,
		Transport: &http.Transport{
			// Proxy 显式为 nil：环境里的 http_proxy/https_proxy 会把请求导向
			// 代理后再由代理解析，拨号层校验形同虚设。
			Proxy:                  nil,
			DialContext:            f.safeDialContext,
			ResponseHeaderTimeout:  cfg.Timeout,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 64 << 10,
		},
	}
	return f
}

// safeDialContext 在实际拨号时解析主机名并逐 IP 校验。任一解析结果被禁即整体
// 拒绝（宁可误伤多 A 记录的站点，也不给攻击者挑中内网记录的机会）。通过校验的
// IP 直接用于拨号，不再把主机名交给系统解析器。
func (f *Fetcher) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, ErrBlocked
	}
	ips, err := f.lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: 解析 %s 失败: %w", ErrNoPreview, host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: 解析 %s 无结果", ErrNoPreview, host)
	}
	for _, ipAddr := range ips {
		if isBlockedIP(ipAddr.IP) {
			return nil, ErrBlocked
		}
	}
	var lastErr error
	for _, ipAddr := range ips {
		// ipAddr.String() 保留 IPv6 zone（JoinHostPort 负责加方括号）；
		// 链路本地地址已在上面的黑名单里拒绝，zone 实际不会走到这里。
		conn, derr := f.dialContext(ctx, network, net.JoinHostPort(ipAddr.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, fmt.Errorf("%w: 拨号失败: %w", ErrNoPreview, lastErr)
}

// checkRedirect 限制跳转次数并对每个 Location 复用同一 URL 校验；私网/非法
// scheme 的重定向在这里就被拒绝，不会进入下一次拨号。域名型的 Location 仍会在
// 下一跳 safeDialContext 里做解析后的 IP 校验。
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return ErrBlocked
	}
	if _, err := validateURL(req.URL.String()); err != nil {
		return err
	}
	return nil
}

// Fetch 抓取 rawURL 的预览。返回的错误可用 errors.Is 区分 ErrBlocked 与
// ErrNoPreview；ErrBlocked 的底层原因（哪一段/哪个 IP）不外传。
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Preview, error) {
	u, err := validateURL(rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrBlocked
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9")
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			return nil, ErrBlocked
		}
		return nil, fmt.Errorf("%w: %v", ErrNoPreview, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrNoPreview
	}
	if !isHTMLContentType(resp.Header.Get("Content-Type")) {
		return nil, ErrNoPreview
	}
	// 多读 1 字节判定超限：正好等于上限的响应仍然接受。
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取响应失败: %v", ErrNoPreview, err)
	}
	if int64(len(body)) > f.maxBytes {
		return nil, ErrNoPreview
	}
	pageURL := u
	if resp.Request != nil && resp.Request.URL != nil {
		pageURL = resp.Request.URL
	}
	p, err := parseHTML(pageURL, body)
	if err != nil {
		return nil, err
	}
	// 图片地址最终复核：服务端从不抓图片，但绝不能把指向内网的 URL 交给
	// 客户端 webview 加载。字面量 IP 已由 validateURL 拦截，域名型地址在这里
	// 解析并逐 IP 校验；任何失败都清空图片而不是让整条预览失败。
	// 残余风险：客户端加载图片时会自行再解析一次 DNS，服务端无法钉住那次
	// 解析结果（DNS 重绑定）；这里挡住的是「服务端明知是私网仍返回」的情况。
	if p.ImageURL != "" && !f.imageURLAllowed(ctx, p.ImageURL) {
		p.ImageURL = ""
	}
	return p, nil
}

// imageURLAllowed 判断图片地址是否可以交给客户端加载：scheme/userinfo/host
// 复用 validateURL；字面量 IP 直接用黑名单判定，域名通过注入的 lookupIP 解析，
// 任一解析结果被禁或解析失败都拒绝。
func (f *Fetcher) imageURLAllowed(ctx context.Context, raw string) bool {
	u, err := validateURL(raw)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if ip := net.ParseIP(host); ip != nil {
		return !isBlockedIP(ip)
	}
	ips, err := f.lookupIP(ctx, host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ipAddr := range ips {
		if isBlockedIP(ipAddr.IP) {
			return false
		}
	}
	return true
}

// isHTMLContentType 只接受 text/html 与 application/xhtml+xml（忽略 charset 等参数）。
func isHTMLContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch strings.ToLower(mt) {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

// resolveImageURL 把页面里的图片地址解析为绝对 URL，并复用 validateURL 的
// scheme/userinfo/字面量 IP 校验。域名解析校验不在这里做（保持 parseHTML 为
// 纯函数、离线可测），由 Fetch 返回前统一经 imageURLAllowed 处理。
func resolveImageURL(page *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	abs := page.ResolveReference(ref)
	if abs == nil {
		return ""
	}
	if _, err := validateURL(abs.String()); err != nil {
		return ""
	}
	return abs.String()
}
