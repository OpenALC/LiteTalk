package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// jwtSecretFile 自动生成密钥的落盘文件名（位于 LITETALK_DATA_DIR）。
const jwtSecretFile = "jwt.secret"

// 并发首启败者的读回重试：胜者用 O_EXCL 建好文件后还要写入内容，
// 败者可能读到空文件或半截内容；总等待上限 5s。CI 满载/race 调度下胜者
// 可能被延迟片刻，1s 会在空文件上误判失败（TestResolveJWTSecretConcurrentFirstStart 踩过）。
const (
	jwtSecretReadRetries  = 500
	jwtSecretReadInterval = 10 * time.Millisecond
)

// resolveJWTSecret 决定 JWT 签名密钥（#27）：
//  1. LITETALK_JWT_SECRET 显式设置优先（生产仍走弱密钥校验）；
//  2. dev 构建保持原行为：空串交给 auth.NewJWT 回落开发密钥；
//  3. 生产构建首次启动生成 32 字节随机密钥并写入 $DATA_DIR/jwt.secret（0600），
//     后续启动读取复用——`--domain` 一条命令即可起服，重启后旧令牌不失效。
//
// 文件用 O_EXCL 创建：多进程同时首启时只有一个写入者，其余进程读回同一份密钥，
// 不会出现「先启动进程持有内存密钥、磁盘上却是另一份」的错位。
func resolveJWTSecret(explicit, dataDir string, dev bool) (secret string, generated bool, err error) {
	if explicit != "" {
		if err := validateJWTSecret(explicit, dev); err != nil {
			return "", false, fmt.Errorf("LITETALK_JWT_SECRET 不合法: %w（请设为至少 32 字节的随机串，如 openssl rand -hex 32）", err)
		}
		return explicit, false, nil
	}
	if dev {
		return "", false, nil
	}

	path := filepath.Join(dataDir, jwtSecretFile)
	secret, err = readJWTSecretFile(path)
	if err == nil {
		return secret, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, fmt.Errorf("创建数据目录 %s 失败: %w", dataDir, err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("生成 JWT 密钥失败: %w", err)
	}
	secret = hex.EncodeToString(buf)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 并发首启的败者：复用胜者写入的密钥。胜者可能刚建好文件还没写完，
			// 短暂重试到内容完整（含「长度不足」这类半截写入的校验失败）。
			for i := 0; i < jwtSecretReadRetries; i++ {
				if i > 0 {
					time.Sleep(jwtSecretReadInterval)
				}
				if secret, rerr := readJWTSecretFile(path); rerr == nil {
					return secret, false, nil
				} else {
					err = rerr
				}
			}
			return "", false, err
		}
		return "", false, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	// umask 会削弱 OpenFile 的 0600，显式 chmod 兜底。
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", false, fmt.Errorf("设置 %s 权限失败: %w", path, err)
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", false, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", false, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return secret, true, nil
}

// readJWTSecretFile 读取并校验持久化密钥；文件缺失返回 fs.ErrNotExist。
// 内容是运维可控的（可预置 secret mount），因此与 env 一样过生产弱密钥校验。
func readJWTSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("JWT 密钥文件 %s 为空（删除该文件可让服务端重新生成）", path)
	}
	if err := validateJWTSecret(secret, false); err != nil {
		return "", fmt.Errorf("JWT 密钥文件 %s 不可用: %w（删除该文件可让服务端重新生成）", path, err)
	}
	return secret, nil
}
