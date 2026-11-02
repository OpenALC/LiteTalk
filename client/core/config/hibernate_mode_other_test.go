//go:build !linux

package config

import (
	"path/filepath"
	"testing"
)

// TestHibernateModeDefaultOther 具体平台断言：非 Linux 平台缺省休眠模式
// 保持 off，不随 GTK4 迁移（Linux 侧改 hibernate，#5）变化。
func TestHibernateModeDefaultOther(t *testing.T) {
	m := NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	if got := m.HibernateMode(); got != "off" {
		t.Fatalf("非 Linux 缺省休眠模式应为 off，得到 %s", got)
	}
}
