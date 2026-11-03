//go:build linux && !gtk3

package config

import "testing"

// TestGTK4BuildDowngrade GTK4 构建（linux && !gtk3）专属断言：
// destroy 不可用、强降生效、其他模式原样。gtk3 侧的镜像断言见
// hibernate_gtk3_test.go；本文件由 GTK4 编译检查 job（W1 落地后的
// check-client-linux）覆盖。
func TestGTK4BuildDowngrade(t *testing.T) {
	if HibernateDestroyAvailable() {
		t.Fatal("GTK4 构建不应报告 destroy 可用（上游窗口销毁泄漏，#5）")
	}
	if got := downgradeDestroyForGTK4("destroy"); got != "hibernate" {
		t.Fatalf("destroy 应强降为 hibernate，得到 %s", got)
	}
	for _, mode := range []string{"off", "hibernate"} {
		if got := downgradeDestroyForGTK4(mode); got != mode {
			t.Fatalf("模式 %s 不应被强降改动，得到 %s", mode, got)
		}
	}
}
