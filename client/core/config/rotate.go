package config

import (
	"fmt"
	"os"
	"sync"
)

// RotateWriter 单文件限长滚动写入器（issue #10，自研替代 lumberjack 等依赖）：
// 当前文件写满 maxBytes 后按 keep-1 个历史档轮换——path → path.1 → path.2，
// 超出 keep 的最旧档删除。keep 含当前文件：keep=3 时磁盘上最多 app.log /
// app.log.1 / app.log.2 三个。启动打开时若已有文件超限同样先滚动（不再截断丢日志）。
// 写入是同步的（log 包本身串行调用）；mu 只防 Close 与 Write 并发。
type RotateWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	perm     os.FileMode
	f        *os.File
	size     int64
}

// NewRotateWriter 打开 path 追加写。maxBytes<=0 或 keep<2 时回落默认
// （5MB × 3）：零值配置必须落到安全默认，不能退化成无界增长。
func NewRotateWriter(path string, maxBytes int64, keep int, perm os.FileMode) (*RotateWriter, error) {
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	if keep < 2 {
		keep = 3
	}
	w := &RotateWriter{path: path, maxBytes: maxBytes, keep: keep, perm: perm}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open 打开当前日志文件并记录尺寸；已有文件超限时先滚动（启动期超限先滚动，
// 不再 Truncate 丢历史）。perm 同时用于收紧旧版本建出的宽权限文件。
func (w *RotateWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, w.perm)
	if err != nil {
		return err
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	_ = f.Chmod(w.perm) // 旧档收紧（Windows 上是只读位，无害）
	if size > w.maxBytes {
		f.Close()
		if err := w.rotate(); err != nil {
			return err
		}
		return nil // rotate 内部重新 open
	}
	w.f = f
	w.size = size
	return nil
}

// Write 满限滚动后写入。单条超过 maxBytes 的超大写入允许落进新档（滚动后直写），
// 不做二次切割——log 行本身有截断上限，这里只保证文件不无界增长。
func (w *RotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, fmt.Errorf("rotate writer closed: %s", w.path)
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭当前文件。幂等。
func (w *RotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotate 轮换一档：最旧档删除，path.N-1 → path.N，path → path.1，再重建空 path。
// 调用方须已关闭 w.f（open 超限分支与 Write 满限分支都满足）。
func (w *RotateWriter) rotate() error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	// 先删最旧，再逐级改名：n=2 是 keep=3 的最旧档下标（app.log.2）。
	oldest := fmt.Sprintf("%s.%d", w.path, w.keep-1)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := w.keep - 2; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		to := fmt.Sprintf("%s.%d", w.path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return w.open()
}
