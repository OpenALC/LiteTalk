//go:build !windows

package main

import "testing"

func TestVoiceStatsOtherPlatformsKeepSnapshotWithoutWindow(t *testing.T) {
	a, emitted := newVoiceStatsTestApp()
	p := &voiceStatsProbe{inRoom: true}
	a.emitVoiceStats(p)
	if p.calls != 1 || *emitted != 1 {
		t.Fatalf("其它平台无窗口时保持原有统计: calls=%d emitted=%d", p.calls, *emitted)
	}
}
