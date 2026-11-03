//go:build !windows

package main

// 其它平台保持原有周期统计行为。
func (a *App) shouldCollectVoiceStats() bool { return true }
