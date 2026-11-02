package main

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type fakeLevelMonitor struct{ stops atomic.Int32 }

func (m *fakeLevelMonitor) Stop() { m.stops.Add(1) }

func newLevelTestApp(t *testing.T) (*App, context.Context) {
	t.Helper()
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	w := &application.WebviewWindow{}
	a.setWindow(w)
	return a, context.WithValue(context.Background(), application.WindowKey, w)
}

func TestLevelMonitorStaleStopCannotCloseNewOwner(t *testing.T) {
	a, ctx := newLevelTestApp(t)
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

	old, err := a.StartLevelMonitor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current, err := a.StartLevelMonitor(ctx)
	if err != nil || current == old || starts != 1 {
		t.Fatalf("新页面应复用设备但取得新 token: old=%q current=%q starts=%d err=%v", old, current, starts, err)
	}
	a.StopLevelMonitor(ctx, old)
	if got := monitors[0].stops.Load(); got != 0 {
		t.Fatalf("旧页面迟到 Stop 误停新页面: %d", got)
	}
	callbacks[0](audio.LevelSample{})
	if got := events.Load(); got != 1 {
		t.Fatalf("当前监视应继续推送电平: %d", got)
	}
	a.StopLevelMonitor(ctx, current)
	a.StopLevelMonitor(ctx, current)
	if got := monitors[0].stops.Load(); got != 1 {
		t.Fatalf("重复 Stop 应只释放一次: %d", got)
	}

	next, err := a.StartLevelMonitor(ctx)
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
	a.StopLevelMonitor(ctx, next)
	if got := monitors[1].stops.Load(); got != 1 {
		t.Fatalf("新监视未释放: %d", got)
	}
}

func TestLevelMonitorLateOldStartDoesNotReplaceNewLease(t *testing.T) {
	a, ctx := newLevelTestApp(t)
	m := &fakeLevelMonitor{}
	starts := 0
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		starts++
		return m, nil
	}
	// A 已发出但尚未被 Wails 调度；B 的调用先执行。
	b, err := a.StartLevelMonitor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aLate, err := a.StartLevelMonitor(ctx)
	if err != nil || aLate == b || starts != 1 {
		t.Fatalf("两次调用应共享设备、各持 lease: a=%q b=%q starts=%d err=%v", aLate, b, starts, err)
	}
	a.StopLevelMonitor(ctx, aLate) // A 的页面已卸载，迟到 Promise 立即归还自己的 lease。
	if got := m.stops.Load(); got != 0 {
		t.Fatalf("A 的迟到停止误停 B: %d", got)
	}
	a.StopLevelMonitor(ctx, b)
	if got := m.stops.Load(); got != 1 {
		t.Fatalf("最后一个 lease 释放后应恰好停一次: %d", got)
	}
}

func TestLevelMonitorOldWindowRejectedAfterResume(t *testing.T) {
	a, oldCtx := newLevelTestApp(t)
	oldMon := &fakeLevelMonitor{}
	newMon := &fakeLevelMonitor{}
	starts := 0
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		starts++
		if starts == 1 {
			return oldMon, nil
		}
		return newMon, nil
	}
	oldToken, err := a.StartLevelMonitor(oldCtx)
	if err != nil {
		t.Fatal(err)
	}
	// destroyRenderProcess 先认领并停监视，随后才 Close/摘除旧窗。旧窗的
	// showFromTray 若在这段间隙晚到，绝不能把旧窗重新视为可启动监视。
	oldWindow := a.getWindow()
	if !a.claimWindowForDestroy(oldWindow) {
		t.Fatal("旧窗口销毁认领失败")
	}
	a.resumeLevelMonitor(oldWindow) // 模拟已持旧窗口引用的唤醒路径在销毁中晚到。
	if _, err := a.StartLevelMonitor(oldCtx); err == nil {
		t.Fatal("销毁中的旧窗口被并发唤醒后不得重开监视")
	}
	a.completeWindowDestroy(oldWindow)
	a.resumeLevelMonitor(oldWindow)
	if got := oldMon.stops.Load(); got != 1 {
		t.Fatalf("销毁旧窗未释放监视: %d", got)
	}
	newWindow := &application.WebviewWindow{}
	a.setWindow(newWindow)
	a.resumeLevelMonitor(newWindow)
	newCtx := context.WithValue(context.Background(), application.WindowKey, newWindow)
	newToken, err := a.StartLevelMonitor(newCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartLevelMonitor(oldCtx); err == nil {
		t.Fatal("旧窗口的迟到 Start 在新窗口恢复后仍须拒绝")
	}
	a.StopLevelMonitor(oldCtx, newToken)
	a.StopLevelMonitor(newCtx, oldToken)
	if got := newMon.stops.Load(); got != 0 {
		t.Fatalf("旧窗口或旧 token 误停新监视: %d", got)
	}
	a.StopLevelMonitor(newCtx, newToken)
	if got := newMon.stops.Load(); got != 1 {
		t.Fatalf("新窗口监视未释放: %d", got)
	}
}

func TestLevelMonitorSecondDestroyCannotClaimRebuiltWindow(t *testing.T) {
	a, oldCtx := newLevelTestApp(t)
	oldMon := &fakeLevelMonitor{}
	newMon := &fakeLevelMonitor{}
	starts := 0
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		starts++
		if starts == 1 {
			return oldMon, nil
		}
		return newMon, nil
	}
	if _, err := a.StartLevelMonitor(oldCtx); err != nil {
		t.Fatal(err)
	}
	oldWindow := a.getWindow()
	// D2 在旧窗口仍是当前窗口时固定目标，随后暂停；D1 先完成销毁。
	releaseD2 := make(chan struct{})
	d2Result := make(chan bool, 1)
	go func(target *application.WebviewWindow) {
		<-releaseD2
		d2Result <- a.claimWindowForDestroy(target)
	}(oldWindow)
	if !a.claimWindowForDestroy(oldWindow) {
		t.Fatal("D1 未能认领旧窗口")
	}
	if a.claimWindowForDestroy(oldWindow) {
		t.Fatal("旧窗口在 D1 未完成时被重复认领")
	}
	a.completeWindowDestroy(oldWindow)
	if got := oldMon.stops.Load(); got != 1 {
		t.Fatalf("D1 应释放旧监视一次: %d", got)
	}
	newWindow := &application.WebviewWindow{}
	a.setWindow(newWindow)
	a.resumeLevelMonitor(newWindow)
	newCtx := context.WithValue(context.Background(), application.WindowKey, newWindow)
	newToken, err := a.StartLevelMonitor(newCtx)
	if err != nil {
		t.Fatal(err)
	}
	close(releaseD2)
	select {
	case claimed := <-d2Result:
		if claimed {
			t.Fatal("D2 把重建的新窗口当作旧目标认领")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("D2 等待窗口认领时死锁")
	}
	if a.getWindow() != newWindow || newMon.stops.Load() != 0 {
		t.Fatal("D2 不得关闭新窗口或停止新监视")
	}
	a.StopLevelMonitor(newCtx, newToken)
	if got := newMon.stops.Load(); got != 1 {
		t.Fatalf("新监视由自己的 lease 释放: %d", got)
	}
}

func TestLevelMonitorDestroyWaitsForInFlightStart(t *testing.T) {
	a, ctx := newLevelTestApp(t)
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
		_, err := a.StartLevelMonitor(ctx)
		started <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("启动未进入测试阻塞点")
	}
	destroyed := make(chan struct{})
	target := a.getWindow()
	go func() {
		if !a.claimWindowForDestroy(target) {
			return
		}
		a.completeWindowDestroy(target)
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
	if _, err := a.StartLevelMonitor(ctx); err == nil {
		t.Fatal("已销毁窗口的迟到启动应拒绝")
	}
	newWindow := &application.WebviewWindow{}
	a.setWindow(newWindow)
	a.resumeLevelMonitor(newWindow)
	newCtx := context.WithValue(context.Background(), application.WindowKey, newWindow)
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		return &fakeLevelMonitor{}, nil
	}
	if token, err := a.StartLevelMonitor(newCtx); err != nil || token == "" {
		t.Fatalf("唤醒后应可重新启动监视: token=%q err=%v", token, err)
	}
	target = a.getWindow()
	if !a.claimWindowForDestroy(target) {
		t.Fatal("新窗口销毁认领失败")
	}
	a.completeWindowDestroy(target)
}

func TestLevelMonitorServiceShutdownStopsAndRejectsLateStart(t *testing.T) {
	a, ctx := newLevelTestApp(t)
	m := &fakeLevelMonitor{}
	a.levelStart = func(_ string, _ audio.InputMode, _ func(audio.LevelSample)) (levelMonitor, error) {
		return m, nil
	}
	token, err := a.StartLevelMonitor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	a.StopLevelMonitor(ctx, token)
	if got := m.stops.Load(); got != 1 {
		t.Fatalf("关机与旧页面 Stop 应只释放一次: %d", got)
	}
	a.resumeLevelMonitor(a.getWindow())
	if _, err := a.StartLevelMonitor(ctx); err == nil {
		t.Fatal("关机后不应再启动独立采集")
	}
}
