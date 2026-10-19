//go:build !windows && !linux

// Package config 的其余平台加密 stub（darwin 等）：
// 无 DPAPI/降级存储，密码不保存（只记邮箱），杜绝明文/假加密落盘。
package config

import "errors"

// protectString 非 Windows 不支持密码加密存储。
func protectString(string) (string, error) {
	return "", errors.New("unsupported platform")
}

// unprotectString 非 Windows 无密文可解。
func unprotectString(string) (string, error) {
	return "", errors.New("unsupported platform")
}

// guestTokenGet 其余平台无机密存储：恒视为未记住（每次新建游客）。
func guestTokenGet(string) string { return "" }

// guestTokenPut 其余平台不保存游客令牌（protectString 恒失败 = 绝不明文落盘），
// 返回 "" 走清空语义。
func guestTokenPut(string) string { return "" }

// secretMode 其余平台无加密后端，按文件弱保护口径上报（不存 = 提示无意义但契约取值固定）。
func secretMode() string { return SecretModeFile }
