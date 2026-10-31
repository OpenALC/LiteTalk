//go:build linux || darwin

package updater

import (
	"os"
	"syscall"
)

// hardenStateDir 启动时收紧已存在但权限过宽的状态目录（#102）。
// 约束：
//   - 目录对 group/other 有任何权限位就收紧到 0700（状态里有部署元数据与去重记录）；
//   - 只 chmod 不 chown：属主不符只告警。安装脚本把目录交给服务用户，若更新器曾被
//     root 跑过，root 在这里 chown 会把属主改回 root，反而让以服务用户运行的更新器
//     拿不到锁——这种修复必须由运维显式做（README §9.4 给了命令）。
//
// 不返回错误：权限修正失败不该阻断更新（后续写 state 会自然报错，且错误信息更直接）。
func hardenStateDir(dir string, warnf func(string, ...any)) {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			warnf("状态目录 %s 权限 %04o 过宽且收紧失败: %v", dir, perm, err)
		} else {
			warnf("状态目录 %s 权限 %04o 过宽，已收紧到 0700", dir, perm)
		}
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		warnf("状态目录 %s 属主 uid=%d 与当前进程 uid=%d 不一致；由服务用户运行更新器时请 sudo chown -R <服务用户>:<服务用户> %s",
			dir, st.Uid, os.Geteuid(), dir)
	}
}

// dirOwnerIsCurrentProcess 供测试断言"属主不符只告警、不改属主"。
func dirOwnerIsCurrentProcess(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Geteuid()
}
