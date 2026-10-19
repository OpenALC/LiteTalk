package main

// 机制性约束：core/ 是与 UI 无关的业务层，禁止反向依赖 desktop / Wails。
// 用标准库 go/ast 扫 core/ 下非测试文件的 import，命中禁用前缀即失败。
// 本测试故意放在 core/ 之外（客户端根包），避免测试文件自己被自己扫到而误禁。

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// banned 核心层禁用的 import 路径前缀：Wails 全家（含 v3 的 pkg/application
// 等子包）、@wailsio 的 Go 侧模块、未来的 desktop 适配层。
var banned = []string{
	"github.com/wailsapp/wails",
	"github.com/wailsio/",
	"github.com/OpenALC/LiteTalk/client/desktop",
}

func TestCoreForbidsDesktopImports(t *testing.T) {
	root := "core"
	var violations []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("解析 %s 失败: %v", path, perr)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if strings.HasPrefix(p, b) {
					violations = append(violations, path+" -> "+p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("%s 下没有扫描到任何非测试 Go 文件，core/ 疑似被搬空", root)
	}
	if len(violations) > 0 {
		t.Fatalf("core/ 禁止 import 桌面层/Wails:\n%s", strings.Join(violations, "\n"))
	}
}
