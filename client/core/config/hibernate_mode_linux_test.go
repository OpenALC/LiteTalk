//go:build linux

package config

import (
	"path/filepath"
	"testing"
)

// TestHibernateModeDefaultLinux 具体平台断言：Linux 缺省休眠模式为
// hibernate（v1.10.0 GTK4 迁移，G1 降级 1，issue #5）。
// gtk3 legacy 构建同样适用——缺省值按平台（linux）区分，不按构建标签。
func TestHibernateModeDefaultLinux(t *testing.T) {
	m := NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	if got := m.HibernateMode(); got != "hibernate" {
		t.Fatalf("Linux 缺省休眠模式应为 hibernate，得到 %s", got)
	}
}
