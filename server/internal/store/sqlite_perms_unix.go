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

// precreateSQLiteFiles 以 0600 预创建库文件与空的 -wal/-shm 文件。
// SQLite 要到首次写入才创建 WAL 文件，且按编译期默认权限（通常 0644）落盘——
// 那时已晚于 OpenSQLite 打开后的收紧步骤。实测（SQLite 3.5x/modernc）提前放好的
// 空 WAL/SHM 会被识别并沿用同一个 inode，因此 0600 权限能保持到实际写入。
// 已存在的文件只打开不截断，崩溃后待恢复的 WAL 不受影响。
func precreateSQLiteFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
