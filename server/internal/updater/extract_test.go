package updater

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeExtractOK(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "a.tar.gz")
	// 打包产物的完整成员集：主服务 + 更新器 + 升级脚本
	blob := buildTar(t, map[string]string{
		"litetalk-server":  "S",
		"litetalk-updater": "U",
		"update.sh":        "#!/bin/sh\n",
	})
	if err := os.WriteFile(tarPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := safeExtract(tarPath, filepath.Join(dir, "x"))
	if err != nil {
		t.Fatalf("正常包应解出: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("解出 %d 个成员, want 3", len(out))
	}
	if got := mustRead(t, out["litetalk-server"]); got != "S" {
		t.Fatalf("内容错误: %q", got)
	}
}

func TestSafeExtractRejects(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		entries []tarEntry
		wantErr string
	}{
		{
			name: "路径穿越",
			entries: []tarEntry{{
				header: tar.Header{Name: "../evil", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg},
				body:   []byte("x"),
			}},
			wantErr: "可疑路径",
		},
		{
			name: "绝对路径",
			entries: []tarEntry{{
				header: tar.Header{Name: "/etc/passwd", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg},
				body:   []byte("x"),
			}},
			wantErr: "可疑路径",
		},
		{
			name: "符号链接",
			entries: []tarEntry{{
				header: tar.Header{Name: "litetalk-server", Linkname: "/bin/sh", Typeflag: tar.TypeSymlink},
			}},
			wantErr: "非常规文件",
		},
		{
			name: "白名单外额外条目",
			entries: []tarEntry{
				{header: tar.Header{Name: "litetalk-server", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg}, body: []byte("x")},
				{header: tar.Header{Name: "evil.sh", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg}, body: []byte("x")},
			},
			wantErr: "白名单外条目",
		},
		{
			name: "重复条目",
			entries: []tarEntry{
				{header: tar.Header{Name: "litetalk-server", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg}, body: []byte("x")},
				{header: tar.Header{Name: "litetalk-server", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg}, body: []byte("y")},
			},
			wantErr: "重复条目",
		},
		{
			name: "缺少主二进制",
			entries: []tarEntry{
				{header: tar.Header{Name: "update.sh", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg}, body: []byte("x")},
			},
			wantErr: "缺少 litetalk-server",
		},
		{
			name: "超大文件",
			entries: []tarEntry{{
				header: tar.Header{Name: "litetalk-server", Mode: 0o755, Size: 5, Typeflag: tar.TypeReg},
				body:   []byte("12345"),
			}},
			wantErr: "体积非法",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tarPath := filepath.Join(dir, strings.ReplaceAll(tc.name, "/", "_")+".tar.gz")
			if err := os.WriteFile(tarPath, buildTarRaw(t, tc.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			// 超大文件用例把上限压到 4 字节触发体积闸，其余按默认上限
			limit := int64(MaxExtractBytes)
			if tc.name == "超大文件" {
				limit = 4
			}
			_, err := safeExtractLimit(tarPath, filepath.Join(dir, "x-"+tc.name), limit)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want 错误含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}
