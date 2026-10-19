package tests

import (
	"os"
	"path/filepath"
	"testing"
)

// findRoot 从测试工作目录（server/api/tests）向上找 api/openapi.yaml 所在的仓库根。
// 契约在仓库根 api/ 下（§13 裁定 1），不随 server 模块走，所以不能用相对固定路径。
func findRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "api", "openapi.yaml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("向上未找到 api/openapi.yaml——契约目录被挪走或测试不在仓库内跑")
		}
		dir = parent
	}
}
