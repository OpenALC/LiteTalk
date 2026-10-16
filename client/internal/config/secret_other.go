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
