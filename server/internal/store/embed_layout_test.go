package store

import (
	"io/fs"
	"testing"

	"github.com/OpenALC/LiteTalk/server/migrations"
)

// TestEmbedLayout 探针：embed 同时含 MySQL 根文件与 sqlite/ 子目录，且 version 同号同名。
func TestEmbedLayout(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	root := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		root[e.Name()] = true
	}
	subFS, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		t.Fatalf("fs.Sub sqlite: %v", err)
	}
	sub := map[string]bool{}
	ses, err := fs.ReadDir(subFS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ses {
		sub[e.Name()] = true
	}
	for name := range root {
		if !sub[name] {
			t.Errorf("sqlite/ 缺少同名文件 %s（version 必须一致）", name)
		}
	}
	for name := range sub {
		if !root[name] {
			t.Errorf("根目录缺少 %s 的 MySQL 方言文件", name)
		}
	}
	if len(root) < 8 {
		t.Fatalf("MySQL 侧迁移文件不足: %d", len(root))
	}
}
