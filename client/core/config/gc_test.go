package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGCProfiles(t *testing.T) {
	want := []GCProfile{
		{GOGC: -1, MemoryLimit: 64 << 20},
		{GOGC: -1, MemoryLimit: 32 << 20},
		{GOGC: -1, MemoryLimit: 18 << 20},
		{GOGC: 100, MemoryLimit: 18 << 20},
		{GOGC: 50, MemoryLimit: 18 << 20},
	}
	for i, profile := range want {
		if got := GCProfileFor(i + 1); got != profile {
			t.Errorf("level %d = %+v, want %+v", i+1, got, profile)
		}
	}
	for _, invalid := range []int{-1, 0, 6} {
		if ValidGCLevel(invalid) || NormalizeGCLevel(invalid) != GCLevelDefault || GCProfileFor(invalid) != want[2] {
			t.Errorf("level %d 未回落中档", invalid)
		}
	}
	if want[2].GOGC != -1 || want[2].MemoryLimit != 18<<20 {
		t.Fatal("中档必须保持既有 off + 18MiB")
	}
}

func TestGCLevelPersistenceAndFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m := NewWithPath(path)
	if got := m.GCLevel(); got != GCLevelDefault {
		t.Fatalf("缺省档位 = %d, want %d", got, GCLevelDefault)
	}
	if err := m.SetGCLevel(5); err != nil {
		t.Fatal(err)
	}
	if got := NewWithPath(path).GCLevel(); got != 5 {
		t.Fatalf("重启后档位 = %d, want 5", got)
	}
	for _, raw := range []string{`{"gc_level":0}`, `{"gc_level":-1}`, `{"gc_level":6}`} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := NewWithPath(path).GCLevel(); got != GCLevelDefault {
			t.Errorf("配置 %s 读取档位 = %d, want 3", raw, got)
		}
	}
	for _, invalid := range []int{0, -1, 6} {
		if err := m.SetGCLevel(invalid); err == nil {
			t.Errorf("写入非法档位 %d 应报错", invalid)
		}
		if got := m.GCLevel(); got != 5 {
			t.Errorf("非法写入后内存档位 = %d, want 5", got)
		}
	}
}

func TestGCLevelSaveFailureDoesNotChangeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	m := NewWithPath(path)
	if err := m.SetGCLevel(5); err == nil {
		t.Fatal("目标是目录时保存应失败")
	}
	if got := m.GCLevel(); got != GCLevelDefault {
		t.Fatalf("保存失败后档位 = %d, want 3", got)
	}
}

func TestConfigUpdatePreservesConcurrentGCLevel(t *testing.T) {
	m := NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	var firstErr, secondErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		firstErr = m.Update(func(c *Config) error {
			close(entered)
			<-release
			c.HomeServerID = "room-1"
			return nil
		})
	}()
	<-entered
	go func() {
		defer wg.Done()
		secondErr = m.SetGCLevel(5)
	}()
	close(release)
	wg.Wait()
	if firstErr != nil || secondErr != nil {
		t.Fatalf("并发保存失败: first=%v second=%v", firstErr, secondErr)
	}
	got := NewWithPath(m.Path()).Get()
	if got.HomeServerID != "room-1" || got.GCLevel != 5 {
		t.Fatalf("并发保存丢设置: %+v", got)
	}
}
