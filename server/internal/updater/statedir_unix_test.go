//go:build linux || darwin

package updater

// 状态目录权限收紧（#102）。Unix 专属：Windows 的 ACL 不映射到 POSIX 权限位，
// 生产平台是 Linux，所以断言只在 Unix 编译并运行（check-server / check-client-linux）。

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog 返回记录写入内容的 logger。
func captureLog() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

// TestOpenStateTightensLooseDir 已存在但 group/other 有权限位（如 0775、0777）的状态目录，
// 启动时收紧到 0700 并记一条日志。
func TestOpenStateTightensLooseDir(t *testing.T) {
	for _, mode := range []os.FileMode{0o775, 0o777, 0o750, 0o705} {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// MkdirAll 受 umask 影响，显式 chmod 到待测权限
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		logger, buf := captureLog()
		if _, err := OpenStateWithLogger(dir, logger); err != nil {
			t.Fatalf("权限 %04o: %v", mode, err)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("权限 %04o 应收紧到 0700，实际 %04o", mode, perm)
		}
		if !strings.Contains(buf.String(), "已收紧到 0700") {
			t.Fatalf("权限 %04o 收紧应有日志，实际 %q", mode, buf.String())
		}
	}
}

// TestOpenStateKeepsTightDir 已经是 0700 的目录不动、也不打告警日志。
func TestOpenStateKeepsTightDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logger, buf := captureLog()
	if _, err := OpenStateWithLogger(dir, logger); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("0700 目录权限被改动: %04o", perm)
	}
	if strings.Contains(buf.String(), "收紧") {
		t.Fatalf("已合规目录不应打收紧日志: %q", buf.String())
	}
}

// TestHardenStateDirDoesNotChown 只 chmod 不 chown：属主不一致时只告警。
// 当前进程即是属主（跨用户场景在 CI 里造不出来，且 chown 需要 root），所以这里
// 断言的是"属主未被改动"和"属主一致时不打属主告警"这两条能本地验证的契约。
func TestHardenStateDirDoesNotChown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	logger, buf := captureLog()
	hardenStateDir(dir, func(format string, args ...any) { logger.Printf(format, args...) })

	if !dirOwnerIsCurrentProcess(dir) {
		t.Fatal("属主不应被改动")
	}
	if strings.Contains(buf.String(), "属主 uid=") {
		t.Fatalf("属主一致时不应告警: %q", buf.String())
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("权限应收紧到 0700，实际 %04o", perm)
	}
}
