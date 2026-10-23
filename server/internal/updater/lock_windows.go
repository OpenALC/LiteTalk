//go:build windows

package updater

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// 生产只部署 Linux；Windows 实现仅为本机编译与测试。
// 这里不用 no-op：本机 go test 也要验证"第二次获取失败"的跨进程锁语义。
// 标准库 syscall 未导出 LockFileEx，用 kernel32 过程地址直调（不引入新依赖）。
const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errnoLockViolation      = syscall.Errno(33) // ERROR_LOCK_VIOLATION
)

var (
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// lockFile 锁文件首字节（独占、非阻塞）。占用的语义与 flock 一致：不同句柄即冲突。
func lockFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0,
		uintptr(unsafe.Pointer(&ol)),
	)
	// 指针以 uintptr 传入系统调用，GC 不跟踪；显式 KeepAlive 防止调用期间被回收。
	runtime.KeepAlive(&ol)
	if r != 0 {
		return nil
	}
	if errors.Is(err, errnoLockViolation) {
		return errLockBlocked
	}
	return err
}

func unlockFile(f *os.File) {
	var ol syscall.Overlapped
	_, _, _ = procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	runtime.KeepAlive(&ol)
}
