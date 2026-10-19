//go:build linux

package config

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// useMockKeyring 切到内存 mock 后端并清探测缓存（MockInit 改的是包级全局
// provider，探测缓存不同步切换会把上一条用例的模式带进本条）。
func useMockKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	resetSecretProbe()
	t.Cleanup(resetSecretProbe)
}

// useBrokenKeyring 模拟无 Secret Service / D-Bus 故障：所有 keyring 调用报错。
func useBrokenKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInitWithError(errors.New("no secret service"))
	resetSecretProbe()
	t.Cleanup(resetSecretProbe)
}

// TestGuestTokenKeyringMode keyring 可用路径：令牌存 Secret Service，
// 文件字段不残留（返回空串），模式上报 keyring。
func TestGuestTokenKeyringMode(t *testing.T) {
	useMockKeyring(t)
	if got := secretMode(); got != SecretModeKeyring {
		t.Fatalf("模式应为 keyring，得到 %s", got)
	}
	if enc := guestTokenPut("rt-abc"); enc != "" {
		t.Fatalf("keyring 成功时文件字段应为空，得到 %q", enc)
	}
	if got := guestTokenGet(""); got != "rt-abc" {
		t.Fatalf("keyring 读回应为 rt-abc，得到 %q", got)
	}
	// 清除：keyring 删除后读回空。
	if enc := guestTokenPut(""); enc != "" {
		t.Fatalf("清除后文件字段应为空，得到 %q", enc)
	}
	if got := guestTokenGet(""); got != "" {
		t.Fatalf("清除后读回应为空，得到 %q", got)
	}
}

// TestGuestTokenFileFallback 不可用路径（无 Secret Service）：回落 base64 文件
// 编码，模式上报 file——UI 据此提示「明文文件存储」，不得静默降级。
func TestGuestTokenFileFallback(t *testing.T) {
	useBrokenKeyring(t)
	if got := secretMode(); got != SecretModeFile {
		t.Fatalf("模式应为 file，得到 %s", got)
	}
	enc := guestTokenPut("rt-xyz")
	if enc == "" {
		t.Fatal("回落文件时应返回可落盘的编码值")
	}
	if got := guestTokenGet(enc); got != "rt-xyz" {
		t.Fatalf("文件编码往返不一致，得到 %q", got)
	}
	if enc := guestTokenPut(""); enc != "" {
		t.Fatalf("清除后文件字段应为空，得到 %q", enc)
	}
}

// TestGuestTokenLegacyFileRead 升级场景：keyring 里还没有令牌但 config.json 存着
// 旧版 base64 值——Get 必须回落读到它，保住同一游客身份（写路径随后把令牌
// 迁进 keyring 并清掉文件字段）。
func TestGuestTokenLegacyFileRead(t *testing.T) {
	useMockKeyring(t)
	legacy, err := protectString("legacy-rt")
	if err != nil {
		t.Fatal(err)
	}
	if got := guestTokenGet(legacy); got != "legacy-rt" {
		t.Fatalf("keyring miss 应回落旧文件值，得到 %q", got)
	}
	// 写路径迁进 keyring 后文件字段应为空。
	if enc := guestTokenPut("legacy-rt"); enc != "" {
		t.Fatalf("迁移写 keyring 后文件字段应为空，得到 %q", enc)
	}
	if got := guestTokenGet(""); got != "legacy-rt" {
		t.Fatalf("迁移后应从 keyring 读到，得到 %q", got)
	}
}

// TestSavedPasswordKeyringMode keyring 可用：登录密码只进 Secret Service，
// EncPassword 留空，config.json 不残留；清除时连 keyring 项一起删。
func TestSavedPasswordKeyringMode(t *testing.T) {
	useMockKeyring(t)
	m := tmpManager(t)
	if err := m.SetSavedCredentials("u@example.com", "p@ssw0rd123"); err != nil {
		t.Fatalf("SetSavedCredentials: %v", err)
	}
	if ec := m.Get().EncPassword; ec != "" {
		t.Fatalf("keyring 可用时 EncPassword 必须留空，得到 %q", ec)
	}
	raw, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := protectString("p@ssw0rd123")
	if strings.Contains(string(raw), "p@ssw0rd123") || strings.Contains(string(raw), legacy) {
		t.Fatalf("config.json 不应残留密码或其 base64: %s", raw)
	}
	if pw, err := keyring.Get(keyringService, keyringPasswordKey); err != nil || pw != "p@ssw0rd123" {
		t.Fatalf("密码应写入 keyring，得到 %q, %v", pw, err)
	}
	c := m.GetSavedCredentials()
	if !c.Saved || c.Email != "u@example.com" || c.Password != "p@ssw0rd123" {
		t.Fatalf("凭据读回异常: %+v", c)
	}
	if err := m.ClearSavedCredentials(); err != nil {
		t.Fatalf("ClearSavedCredentials: %v", err)
	}
	if _, err := keyring.Get(keyringService, keyringPasswordKey); err == nil {
		t.Fatal("清除后 keyring 里不应再有密码")
	}
	if c := NewWithPath(m.Path()).GetSavedCredentials(); c.Saved || c.Password != "" {
		t.Fatalf("清除后不应返回已存凭据: %+v", c)
	}
}

// TestSavedPasswordNoFileFallback keyring 不可用：密码宁可不记住，绝不以 base64
// 写进 config.json；游客令牌仍按原有逻辑回落文件（两者互不影响）。
func TestSavedPasswordNoFileFallback(t *testing.T) {
	useBrokenKeyring(t)
	m := tmpManager(t)
	if err := m.SetSavedCredentials("u@example.com", "p@ssw0rd123"); err != nil {
		t.Fatalf("SetSavedCredentials: %v", err)
	}
	if enc := m.Get().EncPassword; enc != "" {
		t.Fatalf("keyring 不可用时不得回落 base64 文件，得到 %q", enc)
	}
	c := m.GetSavedCredentials()
	if !c.Saved || c.Email != "u@example.com" {
		t.Fatalf("邮箱仍应记住: %+v", c)
	}
	if c.Password != "" {
		t.Fatalf("keyring 不可用时密码应读回空，得到 %q", c.Password)
	}
	// 游客令牌保持既有回落：base64 文件编码可往返。
	if err := m.SetGuestRefresh("rt-fallback"); err != nil {
		t.Fatalf("SetGuestRefresh: %v", err)
	}
	if enc := m.Get().EncGuestRefresh; enc == "" {
		t.Fatal("游客令牌应回落文件编码")
	}
	if got := m.GetGuestRefresh(); got != "rt-fallback" {
		t.Fatalf("游客令牌往返不一致，得到 %q", got)
	}
}

// TestSavedPasswordEmptyClears 空密码保存等同清除：keyring 里的旧密码必须一并删除，
// 否则 config.json 字段清空后旧密码仍会在下次启动复活。
func TestSavedPasswordEmptyClears(t *testing.T) {
	useMockKeyring(t)
	m := tmpManager(t)
	if err := m.SetSavedCredentials("u@example.com", "old-pw"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSavedCredentials("u@example.com", ""); err != nil {
		t.Fatalf("SetSavedCredentials 空密码: %v", err)
	}
	if _, err := keyring.Get(keyringService, keyringPasswordKey); err == nil {
		t.Fatal("空密码保存后 keyring 里不应再有密码")
	}
	if c := m.GetSavedCredentials(); c.Password != "" {
		t.Fatalf("空密码保存后不应读回旧密码，得到 %q", c.Password)
	}
}

// TestSavedPasswordLegacyFileRead 升级兼容：旧版把密码 base64 存在 EncPassword，
// 新版本先读 keyring、miss 后仍能读到旧值；下一次保存成功后迁入 keyring 并清空文件字段。
func TestSavedPasswordLegacyFileRead(t *testing.T) {
	useMockKeyring(t)
	m := tmpManager(t)
	legacy, err := protectString("legacy-pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.SavedEmail = "old@example.com"
		c.EncPassword = legacy
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := NewWithPath(m.Path()).GetSavedCredentials()
	if c.Password != "legacy-pw" {
		t.Fatalf("keyring miss 应回落旧版 base64 密码，得到 %q", c.Password)
	}
	// 下次保存：写入 keyring，文件字段清空。
	if err := m.SetSavedCredentials("old@example.com", "legacy-pw"); err != nil {
		t.Fatalf("SetSavedCredentials: %v", err)
	}
	if enc := m.Get().EncPassword; enc != "" {
		t.Fatalf("迁移到 keyring 后文件字段应为空，得到 %q", enc)
	}
	if pw, err := keyring.Get(keyringService, keyringPasswordKey); err != nil || pw != "legacy-pw" {
		t.Fatalf("迁移后密码应在 keyring，得到 %q, %v", pw, err)
	}
}
