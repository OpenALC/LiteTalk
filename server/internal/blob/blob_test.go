package blob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewSelectsDriver(t *testing.T) {
	dir := t.TempDir()
	if _, driver, err := New(Config{Dir: dir}, nil); err != nil || driver != "local" {
		t.Fatalf("空 Driver 应回落 local: %q %v", driver, err)
	}
	if _, driver, err := New(Config{Driver: "LOCAL", Dir: dir}, nil); err != nil || driver != "local" {
		t.Fatalf("大小写不敏感选择 local: %q %v", driver, err)
	}
	if _, _, err := New(Config{Driver: "ftp", Dir: dir}, nil); err == nil {
		t.Fatal("未知驱动应报错")
	}
}

func TestLocalRequiresDir(t *testing.T) {
	for _, cfg := range []Config{{}, {Driver: "local"}, {Driver: "local", Dir: "   "}} {
		if _, _, err := New(cfg, nil); err == nil {
			t.Fatalf("缺少 Dir 应报错: %+v", cfg)
		}
	}
}

func TestLocalPutGetDelete(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "nested", "blobs") // 不存在：Put 负责创建
	st, driver, err := New(Config{Driver: "local", Dir: dir}, nil)
	if err != nil || driver != "local" {
		t.Fatalf("New: %q %v", driver, err)
	}

	key := "123.thumb.jpg"
	if err := st.Put(ctx, key, []byte("hello"), "image/jpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.Get(ctx, key)
	if err != nil || string(got) != "hello" {
		t.Fatalf("Get = %q %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, key))
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Fatalf("对象权限 = %o，期望 644", perm)
		}
	}

	// 覆盖写：旧内容不残留。
	if err := st.Put(ctx, key, []byte("hello-2"), "image/jpeg"); err != nil {
		t.Fatalf("Put(覆盖): %v", err)
	}
	if got, err := st.Get(ctx, key); err != nil || string(got) != "hello-2" {
		t.Fatalf("覆盖后 Get = %q %v", got, err)
	}

	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get 应 ErrNotFound，实际 %v", err)
	}
	// 幂等：重复删除与删除从未存在的 key 都成功。
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("重复 Delete 应成功: %v", err)
	}
	if err := st.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("删除不存在的 key 应成功: %v", err)
	}
}

func TestLocalRejectsTraversalKeys(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "blobs")
	st, _, err := New(Config{Dir: dir}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	badKeys := []string{
		"",
		".",
		"..",
		"../escape",
		"../../etc/passwd",
		"a/b",
		`a\b`,
		"C:evil",
		"a b",
		"a..b",
		"a*b",
		"a" + string(byte(0)) + "b",
		strings.Repeat("a", maxKeyLen+1),
	}
	for _, key := range badKeys {
		if err := st.Put(ctx, key, []byte("x"), "text/plain"); err == nil {
			t.Fatalf("Put 应拒绝 key %q", key)
		}
		if _, err := st.Get(ctx, key); err == nil {
			t.Fatalf("Get 应拒绝 key %q", key)
		}
		if err := st.Delete(ctx, key); err == nil {
			t.Fatalf("Delete 应拒绝 key %q", key)
		}
	}
	// 所有非法写入都没触达磁盘：根目录不该被创建。
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("非法 key 不应创建存储目录: %v", err)
	}
}

func TestNewS3Validation(t *testing.T) {
	valid := Config{
		Driver:    "s3",
		Endpoint:  "minio.example.com:9000",
		Bucket:    "litetalk",
		AccessKey: "ak",
		SecretKey: "sk",
		Region:    "us-east-1",
		PathStyle: true,
	}
	st, driver, err := New(valid, nil)
	if err != nil || driver != "s3" || st == nil {
		t.Fatalf("合法配置应构造 s3（不联网）: %q %v", driver, err)
	}

	cases := map[string]Config{
		"缺 endpoint": func() Config { c := valid; c.Endpoint = " "; return c }(),
		"缺 bucket":   func() Config { c := valid; c.Bucket = ""; return c }(),
		"缺 access":   func() Config { c := valid; c.AccessKey = ""; return c }(),
		"缺 secret":   func() Config { c := valid; c.SecretKey = ""; return c }(),
	}
	for name, cfg := range cases {
		if _, _, err := New(cfg, nil); err == nil {
			t.Fatalf("%s：应报错", name)
		}
	}
}
