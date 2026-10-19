//go:build !windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// tightenSQLitePerms 收紧数据目录与库文件权限（#88）：目录 0700、库文件及
// -wal/-shm/-journal 0600。旧部署里已存在 0755/0644 的文件会在打开时就地收紧，
// 不需要手工 chmod。
//
// WAL/SHM 可能尚未生成（Ping 后通常存在，但清理/回放期可能缺一个），缺失跳过；
// 真正无法 chmod（权限不足、只读挂载）则报错——密码哈希保持全局可读是安全缺陷，
// 不能降级成告警继续启动。
func tightenSQLitePerms(dir, dbPath string) error {
	// "." 是当前工作目录（常见于只给文件名的调试场景），不归本函数管，绝不 chmod。
	if dir != "" && dir != "." && dir != dbPath {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("sqlite: 收紧数据目录权限 %s: %w", dir, err)
		}
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		if err := os.Chmod(p, 0o600); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("sqlite: 收紧文件权限 %s: %w", p, err)
		}
	}
	return nil
}

// precreateSQLiteFile 以 0600 预创建空库文件（0 字节是合法的空 SQLite 库）。
// 已存在的文件只打开不截断；之后 OpenSQLite 的 tightenSQLitePerms 仍会兜底收紧。
func precreateSQLiteFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
