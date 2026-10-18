// openapigen 从 api/openapi.yaml 生成服务端 REST 接口与前端 TS 类型。
// 契约是唯一事实来源：改 REST 形状必须先改 openapi.yaml，再跑本生成器，
// 禁止手改 *.gen.go / types.gen.ts。幂等：连续跑两次输出不变。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	specPath := flag.String("spec", "", "OpenAPI 文件路径（默认 <root>/api/openapi.yaml）")
	goOut := flag.String("go-out", "", "Go 生成物路径（默认 <root>/server/internal/api/api.gen.go）")
	skipTS := flag.Bool("skip-ts", false, "只生成 Go（本地无 node_modules 时用；CI 不许）")
	flag.Parse()

	root, err := findRoot()
	if err != nil {
		fatal(err)
	}
	spec := *specPath
	if spec == "" {
		spec = filepath.Join(root, "api", "openapi.yaml")
	}
	goTarget := *goOut
	if goTarget == "" {
		goTarget = filepath.Join(root, "server", "internal", "api", "api.gen.go")
	}
	if err := genGo(spec, goTarget); err != nil {
		fatal(err)
	}
	fmt.Printf("openapigen: %s\n", rel(root, goTarget))
	if *skipTS {
		return
	}
	if err := genTS(root, spec); err != nil {
		fatal(err)
	}
	fmt.Printf("openapigen: %s\n", rel(root, filepath.Join(root, "client", "frontend", "src", "api", "types.gen.ts")))
}

// findRoot 从当前目录向上找 api/openapi.yaml 所在的仓库根（含 testdata 场景）。
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "api", "openapi.yaml")); err == nil {
			return dir, nil
		}
		// 生成器可从 api/tools 下运行（实验/spec 未落地时以 client 存在为根信号）。
		if _, err := os.Stat(filepath.Join(dir, "client", "frontend", "package.json")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "server", "go.mod")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("找不到仓库根（api/openapi.yaml）")
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "openapigen:", err)
	os.Exit(1)
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	if runtime.GOOS == "windows" {
		return filepath.ToSlash(r)
	}
	return r
}

// genTS 通过前端包内锁定版本的 openapi-typescript 生成 types.gen.ts。
func genTS(root, spec string) error {
	fe := filepath.Join(root, "client", "frontend")
	cmd := exec.Command("pnpm", "exec", "openapi-typescript", spec,
		"-o", filepath.Join(fe, "src", "api", "types.gen.ts"))
	cmd.Dir = fe
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
