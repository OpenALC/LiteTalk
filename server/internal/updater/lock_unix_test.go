//go:build linux || darwin

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}
	defer l1.Release()

	// 同进程第二次获取也必须失败（flock 以 open file description 为粒度）
	l2, err := AcquireLock(dir)
	if l2 != nil {
		l2.Release()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("第二次获取 = %v, want ErrLocked", err)
	}

	// 释放后可再次获取
	l1.Release()
	l3, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("释放后重获: %v", err)
	}
	l3.Release()

	// 锁文件保留（删除会引入竞态），权限 0600
	fi, err := os.Stat(filepath.Join(dir, "updater.lock"))
	if err != nil {
		t.Fatalf("锁文件应保留: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("锁文件权限 = %o, want 0600", perm)
	}
}

func TestLockReleaseNilSafe(t *testing.T) {
	var l *Lock
	l.Release() // 不得 panic
	l.Release()
}
