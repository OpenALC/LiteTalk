//go:build windows

package updater

// hardenStateDir Windows 上无 POSIX 权限位语义：ACL 由系统管理，这里不做处理。
// 生产平台是 Linux，Unix 侧的收紧逻辑在 statedir_unix.go。
func hardenStateDir(string, func(string, ...any)) {}
