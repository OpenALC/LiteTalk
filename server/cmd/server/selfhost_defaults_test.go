package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSelfHostBuildDefaults 静态检查仓库根 Dockerfile / docker-compose.yml 的自托管默认值（#27）：
// 默认构建不得落到 dev 版本（否则生产守卫全部失效），/data 必须走命名卷并归 uid 65532。
// 本机无 docker 时无法实际构建镜像，这里是防回归的静态护栏。
func TestSelfHostBuildDefaults(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		return string(b)
	}

	dockerfile := read("Dockerfile")
	if regexp.MustCompile(`(?m)^\s*ARG\s+VERSION=dev\s*$`).MatchString(dockerfile) {
		t.Fatal("Dockerfile 的 ARG VERSION 默认值不能是 dev")
	}
	if !strings.Contains(dockerfile, "ARG VERSION=local") {
		t.Fatal("Dockerfile 的 ARG VERSION 默认值应为 local")
	}
	if !strings.Contains(dockerfile, "COPY --from=build --chown=65532:65532 /out/data /data") {
		t.Fatal("镜像内 /data 应以 uid 65532 创建（命名卷继承属主）")
	}

	compose := read("docker-compose.yml")
	if strings.Contains(compose, "VERSION:-dev") {
		t.Fatal("compose 的 VERSION 默认值不能是 dev")
	}
	if !strings.Contains(compose, "VERSION:-local") {
		t.Fatal("compose 的 VERSION 默认值应为 local")
	}
	if regexp.MustCompile(`(?m)^\s*-\s*\./data:/data\s*$`).MatchString(compose) {
		t.Fatal("compose 不应 bind mount ./data（非 root 镜像写不进去），应使用命名卷")
	}
	if !strings.Contains(compose, "- litetalk-data:/data") {
		t.Fatal("compose 应把命名卷 litetalk-data 挂到 /data")
	}
}
