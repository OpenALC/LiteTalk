// Package webui 内嵌 Web 客户端构建产物（#50）。
//
// 仓库常态只提交 dist/.gitkeep 占位（根 .gitignore 有对应例外），保证没有
// 前端产物时 go build / go test 照常通过；CI 的 package-server 在编译前把
// web 构建产物解到 dist/ 再交叉编译，发布二进制里才有真实前端。
// 本包不写任何逻辑——FS 访问口径（SPA fallback、缓存头）统一在
// internal/api/static.go，避免两处各自维护 dist 内的路径知识。
package webui

import "embed"

// dist 内嵌产物根。all: 前缀保底带下划线/点号文件（含 .gitkeep 占位）。
//
//go:embed all:dist
var dist embed.FS

// Dist 返回内嵌产物（顶层目录是 dist/，调用方按需 fs.Sub）。
func Dist() embed.FS { return dist }
