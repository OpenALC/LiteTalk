//go:build linux

// Package config 的 Linux 机密存储降级（问题 D2.7 的前置最小实现）：
// 尚无 Keyring/libsecret 对接（批次 3 引入 go-keyring 后替换），
// 采用「base64 可逆编码 + config.json 0600 权限」的弱保护——
// 与 SSH 私钥同级的文件权限模型，防随手翻看，不防拿到文件的攻击者。
// 游客 refresh_token 因此能跨启动保持，避免每次新建游客账号堆积。
package config

import "encoding/base64"

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
