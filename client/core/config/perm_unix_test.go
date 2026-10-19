//go:build !windows

// 权限位断言（issue #10）：Unix 权限语义只在非 Windows 有意义——Go 在 Windows 上
// 的 Stat().Perm() 只反映只读属性（恒 0666/0444），无法表达 0600/0700。
// Windows 路径由 rotate_test.go 覆盖滚动语义；权限收紧本体是 Unix 硬化。
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRotateWriterPerm 新建日志文件 0600（含对旧宽权限档的启动收紧）。
func TestRotateWriterPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// 旧版本建的 0644 档：打开时应被收紧到 0600。
	if err := os.WriteFile(path, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotateWriter(path, 1<<20, 3, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("app.log 权限应为 0600，得到 %04o", got)
	}
}

// TestConfigDirPerm 配置目录 0700（旧 0755 目录启动收紧）。
func TestConfigDirPerm(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "LiteTalk")
	// 旧版本用 0755 建的目录：Save 后应收紧。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewWithPath(filepath.Join(dir, "config.json"))
	if err := m.SetCloseToTray(true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("配置目录权限应为 0700，得到 %04o", got)
	}
}
