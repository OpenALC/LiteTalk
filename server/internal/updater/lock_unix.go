//go:build linux || darwin

package updater

import (
	"errors"
	"os"
	"syscall"
)

// lockFile 对 fd 加非阻塞排他 flock。flock 以 open file description 为粒度：
// 同一进程再次 open + flock 同样会冲突，这正是"进程内 + 跨进程"单飞都要的语义。
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockBlocked
	}
	return err
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
