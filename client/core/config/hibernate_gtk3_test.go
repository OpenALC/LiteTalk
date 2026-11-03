//go:build linux && gtk3

package config

import "testing"

// TestGTK3BuildDestroyKept gtk3 legacy 构建的镜像断言：destroy 仍可用、
// 读路径原样返回——强降只发生在 GTK4 构建（#5），legacy 行为不变。
func TestGTK3BuildDestroyKept(t *testing.T) {
	if !HibernateDestroyAvailable() {
		t.Fatal("gtk3 构建应报告 destroy 可用")
	}
	if got := downgradeDestroyForGTK4("destroy"); got != "destroy" {
		t.Fatalf("gtk3 构建 destroy 不应被强降，得到 %s", got)
	}
}
