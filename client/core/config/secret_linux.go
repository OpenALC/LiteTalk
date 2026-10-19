//go:build linux

// Package config 的 Linux 机密存储（issue #8）：优先 Secret Service
// （zalando/go-keyring，D-Bus org.freedesktop.secrets），不可用（无桌面密钥环 /
// 无 D-Bus）时回落「base64 编码 + 0600 文件」的弱保护——与 SSH 私钥同级的文件
// 权限模型，防随手翻看，不防拿到文件的攻击者。游客 refresh_token 因此能跨启动
// 保持，避免每次新建游客账号堆积。回落必须让 UI 可感知（secretMode），不静默降级。
package config

import (
	"encoding/base64"
	"errors"
	"sync"

	"github.com/zalando/go-keyring"
)

// keyringService / keyringGuestKey Secret Service 的定位键（固定值，勿改——
// 改了老用户密钥环里的令牌就找不回来）。
const (
	keyringService  = "LiteTalk"
	keyringGuestKey = "guest_refresh"
)

// protectString base64 编码落盘（可逆，非加密；文件权限见 config.go 的 0600）。
func protectString(s string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(s)), nil
}

// unprotectString 还原 protectString 的编码；旧配置里的空/非法值按无值处理。
func unprotectString(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// guestTokenGet 读取游客 refresh_token：优先 Secret Service，miss/故障回落
// fileEnc（config.json 的 EncGuestRefresh，base64 编码）。旧版存的文件值在下一次
// guestTokenPut 成功写 keyring 时被清掉（写路径 keyring 成功即不落文件）。
func guestTokenGet(fileEnc string) string {
	if tok, err := keyring.Get(keyringService, keyringGuestKey); err == nil {
		return tok
	}
	if fileEnc == "" {
		return ""
	}
	plain, err := unprotectString(fileEnc)
	if err != nil {
		return ""
	}
	return plain
}

// guestTokenPut 保存（plain 非空）或清除（空）游客 refresh_token，返回应写进
// EncGuestRefresh 的值。keyring 成功时返回 ""——令牌在密钥环里，文件不残留。
// keyring 故障回落 base64 文件编码；两路都失败返回 ""（绝不明文落盘）。
func guestTokenPut(plain string) string {
	if plain == "" {
		_ = keyring.Delete(keyringService, keyringGuestKey) // 不存在也算清干净
		return ""
	}
	if err := keyring.Set(keyringService, keyringGuestKey, plain); err == nil {
		return ""
	}
	enc, err := protectString(plain)
	if err != nil {
		return ""
	}
	return enc
}

// secretProbe Secret Service 可用性：启动时探测一次供 UI 提示（数据路径每次都
// 直接试 keyring，故障可自愈回落，不依赖这个缓存）。
var (
	probeMu      sync.Mutex
	probeChecked bool
	probeOK      bool
)

// secretMode 当前机密存储模式。探测用只读 Get：ErrNotFound 说明后端只是没存过
// （可用）；其他错误（无 D-Bus / 无 Secret Service）→ 回落文件。
func secretMode() string {
	probeMu.Lock()
	defer probeMu.Unlock()
	if !probeChecked {
		probeChecked = true
		_, err := keyring.Get(keyringService, keyringGuestKey)
		probeOK = err == nil || errors.Is(err, keyring.ErrNotFound)
	}
	if probeOK {
		return SecretModeKeyring
	}
	return SecretModeFile
}

// resetSecretProbe 清探测缓存（测试切换 keyring 后端后必须调用，否则模式读数过期）。
func resetSecretProbe() {
	probeMu.Lock()
	defer probeMu.Unlock()
	probeChecked = false
	probeOK = false
}
