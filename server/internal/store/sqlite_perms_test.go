//go:build !windows

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// assertPerm 断言 POSIX 权限位（#88 的验收面；Windows 由 build tag 跳过）。
func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %04o，期望 %04o", path, got, want)
	}
}

// TestOpenSQLiteTightensExistingLoosePermissions 覆盖旧部署：目录 0755、库文件
// 0644 打开后必须收紧为 0700 / 0600（含已存在的 WAL/SHM）。
func TestOpenSQLiteTightensExistingLoosePermissions(t *testing.T) {
	t.Setenv("LITETALK_SQLITE_PATH", "")
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "litetalk.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}

	sq, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer sq.Close()

	assertPerm(t, dir, 0o700)
	assertPerm(t, dbPath, 0o600)
	// 预创建的 WAL/SHM 在打开后就必须存在且为 0600（SQLite 会沿用同一 inode）。
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		assertPerm(t, p, 0o600)
	}

	// 真实写入后再次确认：WAL 是首次写入才写内容的，权限不能在这时回落到 umask。
	if _, err := sq.DB().Exec("CREATE TABLE perm_probe (x INTEGER)"); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := sq.DB().Exec("INSERT INTO perm_probe VALUES (1)"); err != nil {
		t.Fatalf("写入: %v", err)
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		assertPerm(t, p, 0o600)
	}
}

// TestOpenSQLiteCreatesDataDir0700 覆盖全新部署：多级数据目录按 0700 创建。
func TestOpenSQLiteCreatesDataDir0700(t *testing.T) {
	t.Setenv("LITETALK_SQLITE_PATH", "")
	dir := filepath.Join(t.TempDir(), "a", "b")
	dbPath := filepath.Join(dir, "litetalk.db")

	sq, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer sq.Close()

	assertPerm(t, dir, 0o700)
	assertPerm(t, dbPath, 0o600)
}

// TestTightenSQLitePermsCoversWALAndSHM 直接覆盖 WAL/SHM 分支（不依赖驱动时序）。
func TestTightenSQLitePermsCoversWALAndSHM(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "litetalk.db")
	paths := []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
	for _, p := range paths {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := tightenSQLitePerms(dir, dbPath); err != nil {
		t.Fatalf("tightenSQLitePerms: %v", err)
	}
	assertPerm(t, dir, 0o700)
	for _, p := range paths {
		assertPerm(t, p, 0o600)
	}
}

// TestTightenSQLitePermsSkipsMissingWAL：WAL/SHM 未生成时不能报错。
func TestTightenSQLitePermsSkipsMissingWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "litetalk.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := tightenSQLitePerms(dir, dbPath); err != nil {
		t.Fatalf("缺 -wal/-shm 不应报错: %v", err)
	}
	assertPerm(t, dbPath, 0o600)
}

// TestTightenSQLitePermsDoesNotTouchCWD：只给文件名（dir="."）时不得 chmod 当前目录。
func TestTightenSQLitePermsDoesNotTouchCWD(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cwd)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "litetalk.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tightenSQLitePerms(".", dbPath); err != nil {
		t.Fatalf("tightenSQLitePerms: %v", err)
	}

	after, err := os.Stat(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("当前工作目录权限被改动：%04o → %04o", before.Mode().Perm(), after.Mode().Perm())
	}
	assertPerm(t, dbPath, 0o600)
}
