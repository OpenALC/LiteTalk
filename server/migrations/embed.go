// Package migrations 以 embed.FS 打包迁移 SQL，消除「手工上传 migrations/」部署坑。
// 布局：根目录 *.sql 为 MySQL 方言；sqlite/ 子目录为 SQLite 方言（同号同名，version 一致）。
// MySQL 加载方用 fs.ReadDir(fsys, ".") + !e.IsDir() 天然跳过 sqlite 目录。
package migrations

import "embed"

//go:embed *.sql sqlite/*.sql
var FS embed.FS
