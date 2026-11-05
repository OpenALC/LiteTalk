package blob

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalScanDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "2001.bin")); err != nil {
		t.Fatal(err)
	}
	st, _, err := newLocal(Config{Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*localStore)
	objects, _, _, err := s.Scan(context.Background(), "", 10)
	if err != nil || len(objects) != 0 {
		t.Fatalf("符号链接不得参与枚举: %+v %v", objects, err)
	}
	if _, err := s.Stat(context.Background(), "2001.bin"); err == nil {
		t.Fatal("符号链接不得通过二次核查")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "foreign" {
		t.Fatalf("外部目标发生变化: %q %v", data, err)
	}
}
