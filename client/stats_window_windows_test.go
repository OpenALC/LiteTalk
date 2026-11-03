//go:build windows

package main

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestVoiceStatsWindowsDestroyAndWake(t *testing.T) {
	a, emitted := newVoiceStatsTestApp()
	p := &voiceStatsProbe{inRoom: true}
	a.emitVoiceStats(p)
	if p.calls != 0 || *emitted != 0 {
		t.Fatalf("无主窗口时不得调用昂贵快照: calls=%d emitted=%d", p.calls, *emitted)
	}
	a.setWindow(&application.WebviewWindow{})
	a.emitVoiceStats(p)
	if p.calls != 1 || *emitted != 1 {
		t.Fatalf("存在窗口时应恢复统计: calls=%d emitted=%d", p.calls, *emitted)
	}
	a.setWindow(nil)
	a.emitVoiceStats(p)
	if p.calls != 1 || *emitted != 1 {
		t.Fatalf("再次销毁窗口后应停止快照: calls=%d emitted=%d", p.calls, *emitted)
	}
	a.setWindow(&application.WebviewWindow{})
	a.emitVoiceStats(p)
	if p.calls != 2 || *emitted != 2 {
		t.Fatalf("新窗口登记后应继续原有统计: calls=%d emitted=%d", p.calls, *emitted)
	}
}
