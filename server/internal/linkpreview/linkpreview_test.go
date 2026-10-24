package linkpreview

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// #42 B4：SSRF 防护的地址判定表。只要这些用例通过，私网/回环/元数据/
// CGNAT/IPv4-mapped 等常见绕过路径都被挡住。
func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// 公网地址
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"172.32.0.1", false}, // 172.16/12 之外
		{"100.128.0.1", false},
		{"2001:4860:4860::8888", false},

		// 回环
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"::1", true},
		// 私网
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// CGNAT
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		// 链路本地与云元数据
		{"169.254.1.1", true},
		{"169.254.169.254", true},
		{"fe80::1", true},
		// 未指定 / 组播 / 保留 / 基准 / 文档
		{"0.0.0.0", true},
		{"::", true},
		{"224.0.0.1", true},
		{"ff02::1", true},
		{"192.0.0.8", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"192.0.2.1", true},
		{"198.51.100.1", true},
		{"203.0.113.1", true},
		// IPv4-mapped 必须按底层 IPv4 判定
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:8.8.8.8", false},
		// 可折算到 IPv4 的过渡/转换地址段：整段拒绝
		{"::10.0.0.1", true},            // IPv4-compatible（::/96）
		{"::169.254.169.254", true},     // IPv4-compatible 云元数据
		{"::8.8.8.8", true},             // IPv4-compatible 整段拒绝
		{"64:ff9b::10.0.0.1", true},     // NAT64 well-known
		{"64:ff9b::a00:1", true},        // NAT64 十六进制写法
		{"64:ff9b::8.8.8.8", true},      // NAT64 整段拒绝
		{"64:ff9b:1::a00:1", true},      // NAT64 local-use
		{"2002:a00:1::", true},          // 6to4 内嵌 10.0.0.1
		{"2002:808:808::1", true},       // 6to4 整段拒绝
		{"2001:4860:4860::8844", false}, // 公网 IPv6 不受影响
	}
	for _, tc := range tests {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("测试夹具非法 IP %q", tc.ip)
		}
		if got := isBlockedIP(ip); got != tc.want {
			t.Errorf("isBlockedIP(%s) = %v，期望 %v", tc.ip, got, tc.want)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("nil IP 必须按禁止处理（fail-closed）")
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"https", "https://example.com/a?b=1", true},
		{"http 带端口", "http://example.com:8080/x", true},
		{"公网字面量 IP", "https://8.8.8.8/", true},
		{"大小写 scheme", "HTTPS://example.com/", true},
		{"空串", "", false},
		{"file", "file:///etc/passwd", false},
		{"ftp", "ftp://example.com/a", false},
		{"javascript", "javascript:alert(1)", false},
		{"userinfo", "http://user:pass@example.com/", false},
		{"仅 userinfo 无密码", "http://user@example.com/", false},
		{"localhost", "http://localhost/", false},
		{"localhost 大小写", "http://LOCALHOST:8080/", false},
		{"localhost 末尾点", "http://localhost./", false},
		{"子域 localhost", "http://foo.localhost/", false},
		{"回环 v4", "http://127.0.0.1/", false},
		{"回环 v6", "http://[::1]/", false},
		{"私网", "http://10.0.0.1/", false},
		{"云元数据", "http://169.254.169.254/latest/meta-data/", false},
		{"CGNAT", "http://100.64.0.1/", false},
		{"IPv4-mapped 回环", "http://[::ffff:127.0.0.1]/", false},
		{"IPv6 链路本地", "http://[fe80::1]/", false},
		{"端口越界", "http://example.com:99999/", false},
		{"超长", "https://example.com/" + strings.Repeat("a", MaxURLLength), false},
	}
	for _, tc := range tests {
		u, err := validateURL(tc.raw)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: validateURL(%q) 意外失败: %v", tc.name, tc.raw, err)
			} else if u == nil {
				t.Errorf("%s: validateURL(%q) 返回 nil URL", tc.name, tc.raw)
			}
			continue
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: validateURL(%q) 错误 = %v，期望 ErrBlocked", tc.name, tc.raw, err)
		}
	}
}

// fakeLookup 固定返回给定 IP 列表。
func fakeLookup(ips ...string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(context.Context, string) ([]net.IPAddr, error) {
		out := make([]net.IPAddr, 0, len(ips))
		for _, s := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(s)})
		}
		return out, nil
	}
}

func TestSafeDialContext(t *testing.T) {
	ctx := context.Background()

	t.Run("私网解析结果拒绝拨号", func(t *testing.T) {
		f := New(Config{})
		f.lookupIP = fakeLookup("127.0.0.1")
		dialed := false
		f.dialContext = func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("不应拨号")
		}
		_, err := f.safeDialContext(ctx, "tcp", "evil.example:443")
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("错误 = %v，期望 ErrBlocked", err)
		}
		if dialed {
			t.Fatal("命中私网解析结果时绝不能调用 dialer")
		}
	})

	t.Run("任一解析结果被禁即拒绝", func(t *testing.T) {
		f := New(Config{})
		f.lookupIP = fakeLookup("93.184.216.34", "10.0.0.1")
		dialed := false
		f.dialContext = func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("不应拨号")
		}
		_, err := f.safeDialContext(ctx, "tcp", "mixed.example:443")
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("错误 = %v，期望 ErrBlocked", err)
		}
		if dialed {
			t.Fatal("多 A 记录中有一条私网时不得拨号")
		}
	})

	t.Run("公网解析结果用校验过的 IP 拨号", func(t *testing.T) {
		f := New(Config{})
		f.lookupIP = fakeLookup("93.184.216.34")
		var gotAddr, gotNetwork string
		probe := errors.New("dial probe")
		f.dialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			gotNetwork, gotAddr = network, addr
			return nil, probe
		}
		_, err := f.safeDialContext(ctx, "tcp", "example.com:443")
		if !errors.Is(err, ErrNoPreview) || !errors.Is(err, probe) {
			t.Fatalf("错误 = %v，期望包装 dial probe 的 ErrNoPreview", err)
		}
		if gotNetwork != "tcp" || gotAddr != "93.184.216.34:443" {
			t.Fatalf("dialer 收到 %s/%s，期望 tcp/93.184.216.34:443", gotNetwork, gotAddr)
		}
	})

	t.Run("IPv6 公网地址加方括号拨号", func(t *testing.T) {
		f := New(Config{})
		f.lookupIP = fakeLookup("2001:4860:4860::8888")
		var gotAddr string
		f.dialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
			gotAddr = addr
			return nil, errors.New("dial probe")
		}
		_, _ = f.safeDialContext(ctx, "tcp", "example.com:8443")
		if gotAddr != "[2001:4860:4860::8888]:8443" {
			t.Fatalf("dialer 地址 = %q", gotAddr)
		}
	})

	t.Run("解析失败不拨号", func(t *testing.T) {
		f := New(Config{})
		lookupErr := errors.New("dns down")
		f.lookupIP = func(context.Context, string) ([]net.IPAddr, error) { return nil, lookupErr }
		dialed := false
		f.dialContext = func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("不应拨号")
		}
		_, err := f.safeDialContext(ctx, "tcp", "example.com:443")
		if !errors.Is(err, ErrNoPreview) || !errors.Is(err, lookupErr) {
			t.Fatalf("错误 = %v，期望包装解析错误的 ErrNoPreview", err)
		}
		if dialed {
			t.Fatal("解析失败时不得拨号")
		}
	})

	t.Run("解析结果为空按失败处理", func(t *testing.T) {
		f := New(Config{})
		f.lookupIP = func(context.Context, string) ([]net.IPAddr, error) { return nil, nil }
		_, err := f.safeDialContext(ctx, "tcp", "example.com:443")
		if !errors.Is(err, ErrNoPreview) {
			t.Fatalf("错误 = %v，期望 ErrNoPreview", err)
		}
	})

	t.Run("非法 addr 拒绝", func(t *testing.T) {
		f := New(Config{})
		if _, err := f.safeDialContext(ctx, "tcp", "example.com"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("错误 = %v，期望 ErrBlocked", err)
		}
	})
}
