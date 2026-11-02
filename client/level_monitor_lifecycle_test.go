package main

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/config"
)

type fakeLevelMonitor struct{ stops atomic.Int32 }

func (m *fakeLevelMonitor) Stop() { m.stops.Add(1) }

func newLevelTestApp(t *testing.T) *App {
	t.Helper()
	return newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
}

func TestLevelMonitorStaleStopCannotCloseNewOwner(t *testing.T) {
	a := newLevelTestApp(t)
	var starts int
	var monitors []*fakeLevelMonitor
	var callbacks []func(audio.LevelSample)
	a.levelStart = func(_ string, _ audio.InputMode, emit func(audio.LevelSample)) (levelMonitor, error) {
		starts++
		m := &fakeLevelMonitor{}
		monitors = append(monitors, m)
		callbacks = append(callbacks, emit)
		return m, nil
	}
	var events atomic.Int32
	a.emitHook = func(name string, _ any) {
		if name == "audio:level" {
			events.Add(1)
		}
	}

	old, err := a.StartLevelMonitor()
	if err != nil {
		t.Fatal(err)
	}
	current, err := a.StartLevelMonitor()
	if err != nil || current == old || starts != 1 {
		t.Fatalf("新页面应复用设备但取得新 token: old=%q current=%q starts=%d err=%v", old, current, starts, err)
	}
	a.StopLevelMonitor(old)
	if got := monitors[0].stops.Load(); got != 0 {
		t.Fatalf("旧页面迟到 Stop 误停新页面: %d", got)
	}
	callbacks[0](audio.LevelSample{})
	if got := events.Load(); got != 1 {
		t.Fatalf("当前监视应继续推送电平: %d", got)
	}
	a.StopLevelMonitor(current)
	a.StopLevelMonitor(current)
	if got := monitors[0].stops.Load(); got != 1 {
		t.Fatalf("重复 Stop 应只释放一次: %d", got)
	}

	next, err := a.StartLevelMonitor()
	if err != nil || next == current || starts != 2 {
		t.Fatalf("停止后应可重新启动: next=%q starts=%d err=%v", next, starts, err)
	}
	callbacks[0](audio.LevelSample{})
	if got := events.Load(); got != 1 {
		t.Fatalf("旧采集回调不得推送到新页面: %d", got)
	}
	callbacks[1](audio.LevelSample{})
	if got := events.Load(); got != 2 {
		t.Fatalf("新采集回调应推送电平: %d", got)
	}
	a.StopLevelMonitor(next)
	if got := monitors[1].stops.Load(); got != 1 {
		t.Fatalf("新监视未释放: %d", got)
	}
}

func TestLevelMonitorDestroyWaitsForInFlightStart(t *testing.T) {
	a := newLevelTestApp(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	m := &fakeLevelMonitor{}
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		close(entered)
		<-release
		return m, nil
	}
	started := make(chan error, 1)
	go func() {
		_, err := a.StartLevelMonitor()
		started <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("启动未进入测试阻塞点")
	}
	destroyed := make(chan struct{})
	go func() {
		a.destroyRenderProcess()
		close(destroyed)
	}()
	close(release)
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("启动未结束")
	}
	select {
	case <-destroyed:
	case <-time.After(5 * time.Second):
		t.Fatal("销毁等待启动时死锁")
	}
	if got := m.stops.Load(); got != 1 {
		t.Fatalf("窗口销毁应释放刚启动的监视: %d", got)
	}
	if _, err := a.StartLevelMonitor(); err == nil {
		t.Fatal("已销毁窗口的迟到启动应拒绝")
	}
	a.resumeLevelMonitor()
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		return &fakeLevelMonitor{}, nil
	}
	if token, err := a.StartLevelMonitor(); err != nil || token == "" {
		t.Fatalf("唤醒后应可重新启动监视: token=%q err=%v", token, err)
	}
	a.destroyRenderProcess()
}

func TestLevelMonitorServiceShutdownStopsAndRejectsLateStart(t *testing.T) {
	a := newLevelTestApp(t)
	m := &fakeLevelMonitor{}
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		return m, nil
	}
	token, err := a.StartLevelMonitor()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	a.StopLevelMonitor(token)
	if got := m.stops.Load(); got != 1 {
		t.Fatalf("关机与旧页面 Stop 应只释放一次: %d", got)
	}
	a.resumeLevelMonitor()
	if _, err := a.StartLevelMonitor(); err == nil {
		t.Fatal("关机后不应再启动独立采集")
	}
}
