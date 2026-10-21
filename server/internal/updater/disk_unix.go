//go:build linux || darwin

package updater

import "syscall"

// platformFreeDisk 可用空间按非特权用户可写的块数（Bavail）算：更新器以 www 运行。
func platformFreeDisk(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
