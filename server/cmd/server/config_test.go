package main

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// clearConfigEnv 清掉所有会影响 loadConfig 的环境变量，保证用例从默认值出发。
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LITETALK_DOMAIN", "LITETALK_BEHIND_PROXY", "LITETALK_DATA_DIR",
		"LITETALK_ACME_CACHE", "LITETALK_ADDR", "LITETALK_JWT_SECRET",
		"LITETALK_SFU_IP", "LITETALK_AUTH_MODE", "LITETALK_GATEWAY_BACKLOG",
	} {
		t.Setenv(k, "")
	}
}

// TestLoadConfigPrecedence flag > env > 默认值（§0.6 裁定 6）。
func TestLoadConfigPrecedence(t *testing.T) {
	clearConfigEnv(t)

	// 全默认：无 ACME、数据目录 ./data、证书缓存跟随数据目录。
	cfg, err := loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.acme || cfg.behindProxy {
		t.Fatal("默认应为纯 HTTP 开发模式")
	}
	if cfg.dataDir != "./data" || cfg.acmeCache != filepath.Join("./data", "autocert") {
		t.Fatalf("dataDir=%q acmeCache=%q", cfg.dataDir, cfg.acmeCache)
	}
	if cfg.addr != defaultAddr {
		t.Fatalf("addr = %q，纯 HTTP 不应改端口", cfg.addr)
	}

	// env 层：LITETALK_DOMAIN 启用 ACME，LITETALK_DATA_DIR 覆盖默认。
	t.Setenv("LITETALK_DOMAIN", "env.example.com")
	t.Setenv("LITETALK_DATA_DIR", "/var/lib/litetalk")
	cfg, err = loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.acme || cfg.domain != "env.example.com" {
		t.Fatalf("env 应启用 ACME: acme=%v domain=%q", cfg.acme, cfg.domain)
	}
	if cfg.dataDir != "/var/lib/litetalk" || cfg.acmeCache != filepath.Join("/var/lib/litetalk", "autocert") {
		t.Fatalf("env dataDir 未生效: %q %q", cfg.dataDir, cfg.acmeCache)
	}

	// flag 层盖过 env。
	cfg, err = loadConfig([]string{"--domain", "flag.example.com", "-data", "C:/lt-data"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.domain != "flag.example.com" || cfg.dataDir != "C:/lt-data" {
		t.Fatalf("flag 应覆盖 env: domain=%q dataDir=%q", cfg.domain, cfg.dataDir)
	}
	if cfg.acmeCache != filepath.Join("C:/lt-data", "autocert") {
		t.Fatalf("acmeCache 应跟随 flag dataDir，实际 %q", cfg.acmeCache)
	}
}

// TestLoadConfigACMEAddrSwitch ACME 且 LITETALK_ADDR 未显式配置 → 自动 :443；显式配置不动。
func TestLoadConfigACMEAddrSwitch(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := loadConfig([]string{"--domain", "voice.example.com"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != ":443" || cfg.addrExplicit {
		t.Fatalf("ACME 默认应切 :443，实际 addr=%q explicit=%v", cfg.addr, cfg.addrExplicit)
	}

	t.Setenv("LITETALK_ADDR", ":8443")
	cfg, err = loadConfig([]string{"--domain", "voice.example.com"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != ":8443" {
		t.Fatalf("显式 LITETALK_ADDR 不许自动改，实际 %q", cfg.addr)
	}
}

// TestLoadConfigBehindProxy --behind-proxy 关闭 ACME；域名仍保留（TURN URL host 用）。
func TestLoadConfigBehindProxy(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := loadConfig([]string{"--domain", "voice.example.com", "--behind-proxy"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.acme {
		t.Fatal("behind-proxy 应关闭 ACME（§0.5 触发条件）")
	}
	if !cfg.behindProxy || cfg.domain != "voice.example.com" {
		t.Fatalf("behindProxy=%v domain=%q", cfg.behindProxy, cfg.domain)
	}
	if cfg.addr != defaultAddr {
		t.Fatalf("behind-proxy 保持 LITETALK_ADDR 纯 HTTP，实际 %q", cfg.addr)
	}

	// env 布尔 1/true 生效。
	t.Setenv("LITETALK_BEHIND_PROXY", "1")
	cfg, err = loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.behindProxy {
		t.Fatal("LITETALK_BEHIND_PROXY=1 应生效")
	}
}

// TestLoadConfigErrors 无法识别的位置参数直接报错。
func TestLoadConfigErrors(t *testing.T) {
	clearConfigEnv(t)
	if _, err := loadConfig([]string{"extra"}, io.Discard); err == nil {
		t.Fatal("多余位置参数应报错")
	}
	if _, err := loadConfig([]string{"-no-such-flag"}, io.Discard); err == nil {
		t.Fatal("未知 flag 应报错")
	}
}

// TestConfigValidate 生产三件套守卫 + auth mode + 域名形态（启动序步骤 2）。
func TestConfigValidate(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// dev 模式放行弱密钥（本地开发回落开发密钥）。
	if err := cfg.validate(true); err != nil {
		t.Fatalf("dev 应放行: %v", err)
	}
	// 生产模式：空 JWT secret 拒绝。
	if err := cfg.validate(false); err == nil || !strings.Contains(err.Error(), "LITETALK_JWT_SECRET") {
		t.Fatalf("生产空密钥应拒绝，实际 %v", err)
	}

	cfg.jwtSecret = strings.Repeat("a", 32)
	cfg.sfuIP = "8.8.8.8"    // 生产拒环回（#22），合法用例先给非环回
	cfg.sfuIPExplicit = true // 直接赋值模拟显式配置，跳过域名推导（#27）
	if err := cfg.validate(false); err != nil {
		t.Fatalf("生产合法配置应放行: %v", err)
	}

	// 生产 SFU IP 环回拒绝（#22）。
	cfg.sfuIP = "127.0.0.1"
	if err := cfg.validate(false); err == nil || !strings.Contains(err.Error(), "LITETALK_SFU_IP") {
		t.Fatalf("生产环回 SFU IP 应拒绝，实际 %v", err)
	}
	cfg.sfuIP = "8.8.8.8"

	// auth mode 只认 full/simple。
	cfg.authMode = "weird"
	if err := cfg.validate(true); err == nil {
		t.Fatal("非法 auth mode 应拒绝")
	}
	cfg.authMode = "full"

	// 域名形态：URL / 带路径 / 带端口拒绝。
	for _, bad := range []string{"https://voice.example.com", "voice.example.com/x", "voice.example.com:443"} {
		cfg.domain = bad
		if err := cfg.validate(true); err == nil {
			t.Fatalf("域名 %q 应拒绝", bad)
		}
	}
	cfg.domain = "voice.example.com"
	if err := cfg.validate(true); err != nil {
		t.Fatalf("合法域名应放行: %v", err)
	}
}

// TestResolveSFUIPFromDomain #27 裁定 14：LITETALK_SFU_IP 未设 + --domain 已设时
// 解析域名推导 SFU 宣告地址；解析失败要求手动设置；显式设置优先；#22 拒环回守卫不变。
func TestResolveSFUIPFromDomain(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("LITETALK_JWT_SECRET", strings.Repeat("a", 32)) // 生产密钥合规，专测 SFU IP 路径
	defer func(old func(string) ([]net.IP, error)) { lookupIP = old }(lookupIP)

	// 推导：域名解析出公网 IP → SFU 宣告地址。
	lookupIP = func(host string) ([]net.IP, error) {
		if host != "voice.example.com" {
			t.Fatalf("解析主机 = %q", host)
		}
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	cfg, err := loadConfig([]string{"-domain", "voice.example.com"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := cfg.validate(false); err != nil {
		t.Fatalf("生产 + 域名推导应放行: %v", err)
	}
	if cfg.sfuIP != "203.0.113.10" {
		t.Fatalf("sfuIP = %q，应为域名解析结果", cfg.sfuIP)
	}

	// 解析失败：报错并提示手动设置 LITETALK_SFU_IP，不静默回落环回。
	lookupIP = func(string) ([]net.IP, error) { return nil, errors.New("nxdomain") }
	cfg2, err := loadConfig([]string{"-domain", "no.such.host.invalid"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	err = cfg2.validate(false)
	if err == nil || !strings.Contains(err.Error(), "LITETALK_SFU_IP") {
		t.Fatalf("解析失败应要求手动设置 LITETALK_SFU_IP，实际 %v", err)
	}

	// 显式 LITETALK_SFU_IP 优先，不触发域名解析。
	lookupIP = func(string) ([]net.IP, error) { return nil, errors.New("不应被调用") }
	t.Setenv("LITETALK_SFU_IP", "198.51.100.7")
	cfg3, err := loadConfig([]string{"-domain", "voice.example.com"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := cfg3.validate(false); err != nil {
		t.Fatalf("显式 SFU IP 应放行: %v", err)
	}
	if cfg3.sfuIP != "198.51.100.7" {
		t.Fatalf("显式设置不应被推导覆盖，实际 %q", cfg3.sfuIP)
	}

	// #22 守卫不变：推导出环回照样拒（域名指到 127.0.0.1 的错误配置不放行）。
	t.Setenv("LITETALK_SFU_IP", "")
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }
	cfg4, err := loadConfig([]string{"-domain", "voice.example.com"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	err = cfg4.validate(false)
	if err == nil || !strings.Contains(err.Error(), "环回") {
		t.Fatalf("推导出环回在生产应拒绝，实际 %v", err)
	}
}

// TestValidatePrintAdminInvite -print-admin-invite 跳过 SFU IP 校验（邀请码找回不被
// DNS/环回问题挡住），但 JWT 密钥仍把关（码由它派生）。
func TestValidatePrintAdminInvite(t *testing.T) {
	clearConfigEnv(t)
	defer func(old func(string) ([]net.IP, error)) { lookupIP = old }(lookupIP)
	lookupIP = func(string) ([]net.IP, error) { return nil, errors.New("nxdomain") }

	cfg, err := loadConfig([]string{"-print-admin-invite", "-domain", "voice.example.com"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// dev：空密钥 + 解析失败的域名也应放行（只打码）。
	if err := cfg.validate(true); err != nil {
		t.Fatalf("print-admin-invite 应跳过 SFU/域名校验: %v", err)
	}
	// 生产：密钥不合法仍拒——派生不出正确码。
	if err := cfg.validate(false); err == nil || !strings.Contains(err.Error(), "LITETALK_JWT_SECRET") {
		t.Fatalf("生产缺密钥应拒绝，实际 %v", err)
	}
}
