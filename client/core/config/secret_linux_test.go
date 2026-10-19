//go:build linux

package config

import (
	"errors"
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
