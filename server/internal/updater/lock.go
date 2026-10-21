package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked 另一个更新进程持有跨进程锁。调用方据此返回 409 / --once 退出 75。
var ErrLocked = errors.New("另一个更新进程正在运行")

// errLockBlocked 平台层的 would-block 哨兵，由 AcquireLock 统一映射为 ErrLocked。
var errLockBlocked = errors.New("锁被占用")

// Lock 跨进程互斥锁（服务进程与 --once/SSH 兜底共用同一把）。
// 进程退出时内核自动释放；因此 Release 不删除锁文件——删除 flock 文件会引入竞态
// （新进程可能拿到旧 inode 的锁，而第三个进程在新 inode 上又加锁成功）。
type Lock struct {
	f *os.File
}

// AcquireLock 在 dir 下取非阻塞排他锁。同一时刻只允许一个更新流水线（下载/替换/重启）。
func AcquireLock(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("建状态目录 %s: %w", dir, err)
	}
	path := filepath.Join(dir, "updater.lock")
	// 锁文件不敏感，但状态目录里全是部署元数据：统一 0600，不给他人读写。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件 %s: %w（检查目录属主与运行用户是否一致）", path, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, errLockBlocked) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("获取跨进程锁 %s: %w（检查锁文件属主与运行用户是否一致）", path, err)
	}
	return &Lock{f: f}, nil
}

// Release 解锁并关闭；nil-safe 且可重复调用（后台 worker 的 defer 与显式释放可能并存）。
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}
