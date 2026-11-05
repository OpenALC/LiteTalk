package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// localStore 本地目录实现：一个 key 一个文件，直接放在根目录下（key 不含分隔符）。
type localStore struct {
	dir        string
	scanMu     sync.Mutex
	scanFile   *os.File
	scanOffset int
}

func (*localStore) RestartAfterDelete() bool { return true }

func (l *localStore) CloseScan() error {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()
	return l.closeScanLocked()
}

func (l *localStore) closeScanLocked() error {
	if l.scanFile == nil {
		return nil
	}
	err := l.scanFile.Close()
	l.scanFile, l.scanOffset = nil, 0
	return err
}

func newLocal(cfg Config, lg *log.Logger) (Store, string, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		// 默认值由调用方（cmd/server）给；这里静默回落会让部署写错目录，必须报错。
		return nil, "", errors.New("blob: local 驱动必须配置 Dir")
	}
	if lg != nil {
		lg.Printf("[blob] local dir=%s", dir)
	}
	return &localStore{dir: dir}, "local", nil
}

// path 校验 key 并拼出绝对/相对路径。key 字符集已排除分隔符与 ".."，拼接触发穿越
// 的情况不存在；这里再确认结果仍在根目录下，防御未来校验被改松。
func (l *localStore) path(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	p := filepath.Join(l.dir, key)
	rel, err := filepath.Rel(l.dir, p)
	if err != nil || rel != key {
		return "", fmt.Errorf("blob: key %q 越出存储根目录", key)
	}
	return p, nil
}

// Put 原子写入：同目录临时文件 0600 写入 + fsync，再 chmod 0644 并 rename 覆盖。
// rename 在同一文件系统内原子，读方要么看到旧对象要么看到完整新对象。
func (l *localStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := l.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob: 建目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("blob: 建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	discard := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		discard()
		return fmt.Errorf("blob: 写临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		discard()
		return fmt.Errorf("blob: 临时文件 fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blob: 关闭临时文件: %w", err)
	}
	// rename 之前落定最终权限，避免对象短暂以 0600 存在导致下载失败。
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blob: 设置对象权限: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blob: 替换对象: %w", err)
	}
	return nil
}

// Get 读取整个对象；不存在返回 ErrNotFound。
func (l *localStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blob: 读取对象: %w", err)
	}
	return data, nil
}

// Delete 删除对象；不存在视为成功（幂等）。
func (l *localStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: 删除对象: %w", err)
	}
	return nil
}

// Scan 用目录流分批读取，内存始终不超过 limit；游标是本轮目录枚举偏移。
// 目录变化可能使本轮漏项，下轮从头枚举补齐。
func (l *localStore) Scan(ctx context.Context, after string, limit int) ([]ObjectInfo, string, bool, error) {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	offset := 0
	if after != "" {
		n, err := strconv.Atoi(after)
		if err != nil || n < 0 {
			return nil, "", false, fmt.Errorf("blob: 无效本地扫描游标")
		}
		offset = n
	}
	if l.scanFile == nil || l.scanOffset != offset {
		if err := l.closeScanLocked(); err != nil {
			return nil, "", false, err
		}
		f, err := os.Open(l.dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", true, nil
		}
		if err != nil {
			return nil, "", false, err
		}
		l.scanFile = f
		left := offset
		for left > 0 {
			if err := ctx.Err(); err != nil {
				_ = l.closeScanLocked()
				return nil, "", false, err
			}
			n := left
			if n > 500 {
				n = 500
			}
			entries, err := l.scanFile.ReadDir(n)
			left -= len(entries)
			if err == io.EOF {
				_ = l.closeScanLocked()
				return nil, "", true, nil
			}
			if err != nil {
				_ = l.closeScanLocked()
				return nil, "", false, err
			}
		}
		l.scanOffset = offset
	}
	entries, err := l.scanFile.ReadDir(limit)
	if err != nil && err != io.EOF {
		_ = l.closeScanLocked()
		return nil, "", false, err
	}
	l.scanOffset += len(entries)
	out := make([]ObjectInfo, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			_ = l.closeScanLocked()
			return nil, "", false, err
		}
		if !e.Type().IsRegular() {
			continue
		}
		info, eerr := e.Info()
		if eerr != nil {
			_ = l.closeScanLocked()
			return nil, "", false, eerr
		}
		out = append(out, ObjectInfo{Key: e.Name(), Modified: info.ModTime(), Size: info.Size()})
	}
	next := strconv.Itoa(offset + len(entries))
	done := err == io.EOF || len(entries) < limit
	if done {
		_ = l.closeScanLocked()
	}
	return out, next, done, nil
}

func (l *localStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	p, err := l.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return ObjectInfo{}, fmt.Errorf("blob: 对象不是普通文件")
	}
	return ObjectInfo{Key: key, Modified: info.ModTime(), Size: info.Size(), Owned: true}, nil
}
