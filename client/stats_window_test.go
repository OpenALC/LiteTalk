package main

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

type voiceStatsProbe struct {
	inRoom bool
	calls  int
}

func (p *voiceStatsProbe) InRoom() bool { return p.inRoom }

func (p *voiceStatsProbe) Stats() string {
	p.calls++
	return `{"members":1}`
}

func newVoiceStatsTestApp() (*App, *int) {
	var emitted int
	a := &App{emitHook: func(name string, data any) {
		if name == "voice:stats" && data == `{"members":1}` {
			emitted++
		}
	}}
	return a, &emitted
}

func TestVoiceStatsNotInRoomSkipsSnapshot(t *testing.T) {
	a, emitted := newVoiceStatsTestApp()
	a.setWindow(&application.WebviewWindow{})
	p := &voiceStatsProbe{}
	a.emitVoiceStats(p)
	if p.calls != 0 || *emitted != 0 {
		t.Fatalf("未入房时不应计算或推送统计: calls=%d emitted=%d", p.calls, *emitted)
	}
}
