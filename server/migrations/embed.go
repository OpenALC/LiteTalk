// Package migrations 以 embed.FS 打包迁移 SQL，消除「手工上传 migrations/」部署坑。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
