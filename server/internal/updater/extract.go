package updater

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 打包产物允许的成员白名单：主服务二进制必有；更新器/升级脚本随包携带但不由本更新器替换
// （更新器自升级走 install.sh，避免更新中改写自身）。
var allowedMembers = map[string]bool{
	"litetalk-server":  true,
	"litetalk-updater": true,
	"update.sh":        true,
}

// safeExtract 解包 tar.gz 到 destDir。安全约束（任一违反即整体失败）：
//   - 只允许白名单内的普通文件，且不得重复；
//   - 拒绝绝对路径、路径穿越、符号链接/硬链接/设备/目录等非常规条目；
//   - 单条目不得超过 MaxExtractBytes（解包炸弹）。
//
// 返回 解出的文件名 -> 落盘路径。
func safeExtract(tarPath, destDir string) (map[string]string, error) {
	return safeExtractLimit(tarPath, destDir, MaxExtractBytes)
}

// safeExtractLimit 同 safeExtract，条目大小上限可指定（测试用小值覆盖超限路径）。
func safeExtractLimit(tarPath, destDir string, maxBytes int64) (map[string]string, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip 头非法: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	out := make(map[string]string, 3)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读 tar: %w", err)
		}
		name := hdr.Name
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "..") ||
			strings.ContainsAny(name, `\`) || strings.Contains(name, "/") {
			return nil, fmt.Errorf("拒绝可疑路径条目 %q", name)
		}
		if !allowedMembers[name] {
			return nil, fmt.Errorf("拒绝白名单外条目 %q", name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("拒绝重复条目 %q", name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("拒绝非常规文件条目 %q（type=%d）", name, hdr.Typeflag)
		}
		if hdr.Size <= 0 || hdr.Size > maxBytes {
			return nil, fmt.Errorf("条目 %q 体积非法（%d 字节）", name, hdr.Size)
		}
		dst := filepath.Join(destDir, name)
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err != nil {
			return nil, err
		}
		// LimitReader 多留 1 字节，防止 tar 头尺寸与实际数据不符时被绕过
		n, err := io.Copy(w, io.LimitReader(tr, hdr.Size+1))
		if err != nil {
			w.Close()
			return nil, fmt.Errorf("解出 %q: %w", name, err)
		}
		if n != hdr.Size {
			w.Close()
			return nil, fmt.Errorf("条目 %q 尺寸与数据不符", name)
		}
		if err := w.Sync(); err != nil {
			w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		out[name] = dst
	}
	if _, ok := out["litetalk-server"]; !ok {
		return nil, fmt.Errorf("压缩包缺少 litetalk-server")
	}
	return out, nil
}
