package config

import "fmt"

const GCLevelDefault = 3

// GCProfile 是 Go runtime 的两项回收参数；负 GOGC 关闭堆增长触发，软上限仍生效。
type GCProfile struct {
	GOGC        int
	MemoryLimit int64
}

var gcProfiles = [...]GCProfile{
	{},
	{GOGC: -1, MemoryLimit: 64 << 20},
	{GOGC: -1, MemoryLimit: 32 << 20},
	{GOGC: -1, MemoryLimit: 18 << 20},
	{GOGC: 100, MemoryLimit: 18 << 20},
	{GOGC: 50, MemoryLimit: 18 << 20},
}

func ValidGCLevel(level int) bool { return level >= 1 && level <= 5 }

func NormalizeGCLevel(level int) int {
	if !ValidGCLevel(level) {
		return GCLevelDefault
	}
	return level
}

func GCProfileFor(level int) GCProfile { return gcProfiles[NormalizeGCLevel(level)] }

func (m *Manager) GCLevel() int { return NormalizeGCLevel(m.Get().GCLevel) }

func (m *Manager) SetGCLevel(level int) error {
	if !ValidGCLevel(level) {
		return fmt.Errorf("GC 强度非法: %d", level)
	}
	return m.Update(func(c *Config) error {
		c.GCLevel = level
		return nil
	})
}
