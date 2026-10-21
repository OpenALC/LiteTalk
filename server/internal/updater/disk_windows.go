//go:build windows

package updater

import (
	"syscall"
	"unsafe"
)

// platformFreeDisk Windows 仅供本机跑测试用（生产平台是 Linux）。
func platformFreeDisk(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	// GetDiskFreeSpaceExW：avail 是调用者配额内的可用量，与 Linux Bavail 口径一致。
	r, _, callErr := procDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if r == 0 {
		return 0, callErr
	}
	return avail, nil
}

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
)
