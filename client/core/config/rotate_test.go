package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kbBlocks 造 nKB 内容，每 1KB 填同一个 tag 字节——各档内容可区分，才能断言
// 轮换真的把旧档挪了位（全填相同字节时「删了又建」与「没动」看起来一样）。
func kbBlocks(tag byte, nKB int) []byte {
	return bytes.Repeat(bytes.Repeat([]byte{tag}, 1024), nKB)
}

// mustWrite 写入并要求全部落盘。
func mustWrite(t *testing.T, w *RotateWriter, data []byte) {
	t.Helper()
	if n, err := w.Write(data); err != nil || n != len(data) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
}

// readGen 读某一档内容；不存在返回 (nil, nil)。
func readGen(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRotateWriterRotation 写满上限后滚动：当前档转 .1，.1 转 .2，最旧的 .2 被删。
// keep=3（含当前），磁盘上始终只有 3 个文件。
func TestRotateWriterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// 4KB 上限，keep=3：每档 4KB。
	w, err := NewRotateWriter(path, 4*1024, 3, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	mustWrite(t, w, kbBlocks('A', 4)) // 档 1
	mustWrite(t, w, kbBlocks('B', 4)) // 满 → 滚出 .1，档 2
	mustWrite(t, w, kbBlocks('C', 4)) // 满 → 滚出 .2，档 3

	if !bytes.Equal(readGen(t, path+".1"), kbBlocks('B', 4)) {
		t.Fatal("app.log.1 应为档 2 的内容 B")
	}
	if !bytes.Equal(readGen(t, path+".2"), kbBlocks('A', 4)) {
		t.Fatal("app.log.2 应为档 1 的内容 A")
	}

	mustWrite(t, w, kbBlocks('D', 4)) // 满 → 最旧 A 删除，B→.2，C→.1，档 4

	if !bytes.Equal(readGen(t, path+".1"), kbBlocks('C', 4)) {
		t.Fatal("滚动后 app.log.1 应为档 3 的内容 C")
	}
	if !bytes.Equal(readGen(t, path+".2"), kbBlocks('B', 4)) {
		t.Fatal("滚动后 app.log.2 应为档 2 的内容 B（最旧 A 已删除）")
	}
	// keep=3：不存在 app.log.3。
	if data := readGen(t, path+".3"); data != nil {
		t.Fatal("不应出现 app.log.3")
	}
}

// TestRotateWriterReopenFailure 轮转后重新打开失败（磁盘满/权限突变）：
// 错误必须传到 Write 调用方，且不能留下指向已关闭文件的半开状态。
func TestRotateWriterReopenFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := NewRotateWriter(path, 4*1024, 3, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	mustWrite(t, w, kbBlocks('A', 4))

	// 首次打开已成功；后续重建当前档一律失败，模拟轮转后的重开失败。
	w.openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errors.New("injected reopen failure")
	}
	n, err := w.Write(kbBlocks('B', 4))
	if err == nil {
		t.Fatal("轮转后重新打开失败应返回错误")
	}
	if n != 0 {
		t.Fatalf("失败写入不应报告写入字节数，得到 %d", n)
	}
	if w.f != nil {
		t.Fatal("重新打开失败后不应保留文件句柄")
	}
	// 轮转本身已完成：旧档已挪到 .1，失败发生在重建当前档。
	if !bytes.Equal(readGen(t, path+".1"), kbBlocks('A', 4)) {
		t.Fatal("重新打开失败前应已完成旧档轮转（app.log.1 为档 1 内容 A）")
	}
}

// TestRotateWriterAppendAndClose 打开不截断已有内容；Close 幂等且之后 Write 报错。
func TestRotateWriterAppendAndClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("old-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotateWriter(path, 1<<20, 3, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, kbBlocks('x', 1))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
	if _, err := w.Write(kbBlocks('y', 1)); err == nil {
		t.Fatal("关闭后 Write 应报错")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "old-line\n") {
		t.Fatal("打开时不应截断已有内容")
	}
}
