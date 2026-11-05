package main

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type wakeCall struct {
	window    *application.WebviewWindow
	built     bool
	maximised bool
	focus     bool
}

func waitWakeCall(t *testing.T, calls <-chan wakeCall) wakeCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("等待窗口唤醒超时")
		return wakeCall{}
	}
}

func waitWakeDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("等待窗口生命周期动作超时")
	}
}

func waitWakeIdle(t *testing.T, a *App) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		a.winMu.Lock()
		idle := a.destroyingWindow == nil && !a.wakeInProgress && !a.wakePending
		a.winMu.Unlock()
		if idle {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("窗口生命周期未归于空闲")
		}
	}
}

func TestWindowsWakeConcurrentBuildReentryAndFocus(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	newWindow := &application.WebviewWindow{}
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := make(chan wakeCall, 3)
	var builds atomic.Int32
	a.wakeBuild = func(opts application.WebviewWindowOptions) *application.WebviewWindow {
		if opts.Width != 1080 || opts.Height != 680 || !opts.Hidden {
			t.Errorf("首次重建窗口尺寸或隐藏态错误: %+v", opts)
		}
		builds.Add(1)
		// Wails 建窗可能重入托盘事件；同一调用栈也不能再次建窗或死锁。
		a.showFromTrayWindows(false)
		close(entered)
		<-release
		return newWindow
	}
	a.wakeActivate = func(win *application.WebviewWindow, built, maximised, focus bool) {
		calls <- wakeCall{win, built, maximised, focus}
	}
	done := make(chan struct{})
	go func() { a.showFromTrayWindows(false); close(done) }()
	waitWakeDone(t, entered)
	a.showFromTrayWindows(false)
	a.ShowMainWindow()
	if got := builds.Load(); got != 1 {
		t.Fatalf("并发唤醒重复建窗: %d", got)
	}
	close(release)
	waitWakeDone(t, done)
	first := waitWakeCall(t, calls)
	second := waitWakeCall(t, calls)
	if first.window != newWindow || !first.built || first.focus || second.window != newWindow || second.built || !second.focus {
		t.Fatalf("重入/单实例聚焦应复用同一窗口: first=%+v second=%+v", first, second)
	}
	if a.getWindow() != newWindow || builds.Load() != 1 {
		t.Fatal("唤醒完成后应仅登记一个主窗口")
	}
}

func TestWindowsWakeDuringDestroyRestoresSavedGeometry(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	oldWindow := &application.WebviewWindow{}
	newWindow := &application.WebviewWindow{}
	a.setWindow(oldWindow)
	enteredClose := make(chan struct{})
	releaseClose := make(chan struct{})
	destroyed := make(chan struct{})
	var closes atomic.Int32
	a.destroySnapshot = func(win *application.WebviewWindow) (application.Rect, bool) {
		if win != oldWindow {
			t.Errorf("读取了非旧窗口的几何态")
		}
		return application.Rect{X: 83, Y: 47, Width: 1234, Height: 777}, true
	}
	a.destroyClose = func(win *application.WebviewWindow) {
		if win != oldWindow {
			t.Errorf("关闭了新窗口")
		}
		closes.Add(1)
		close(enteredClose)
		<-releaseClose
	}
	options := make(chan application.WebviewWindowOptions, 1)
	calls := make(chan wakeCall, 1)
	a.wakeBuild = func(opts application.WebviewWindowOptions) *application.WebviewWindow {
		options <- opts
		return newWindow
	}
	a.wakeActivate = func(win *application.WebviewWindow, built, maximised, focus bool) {
		calls <- wakeCall{win, built, maximised, focus}
	}
	go func() { a.destroyRenderProcessFor(oldWindow); close(destroyed) }()
	waitWakeDone(t, enteredClose)
	if !a.recreating.Load() {
		t.Fatal("程序化关闭期间应处于 recreating")
	}
	a.showFromTrayWindows(false)
	a.ShowMainWindow()
	select {
	case <-options:
		t.Fatal("旧窗口关闭完成前不能重建")
	default:
	}
	close(releaseClose)
	waitWakeDone(t, destroyed)
	opts := func() application.WebviewWindowOptions {
		select {
		case v := <-options:
			return v
		case <-time.After(5 * time.Second):
			t.Fatal("关闭后未执行排队的唤醒")
			return application.WebviewWindowOptions{}
		}
	}()
	if opts.X != 83 || opts.Y != 47 || opts.Width != 1234 || opts.Height != 777 || !opts.Hidden {
		t.Fatalf("重建未恢复原窗口位置和尺寸: %+v", opts)
	}
	call := waitWakeCall(t, calls)
	if call.window != newWindow || !call.built || !call.maximised || !call.focus {
		t.Fatalf("重建未恢复最大化或单实例聚焦: %+v", call)
	}
	if a.recreating.Load() || a.getWindow() != newWindow {
		t.Fatal("旧窗口关闭后应复位 recreating 并登记新窗口")
	}
	// 原窗口的迟到最小化/关闭事件不能认领、关闭或隐藏新窗口。
	a.destroyRenderProcessFor(oldWindow)
	if a.interceptCloseForWindow(oldWindow) || closes.Load() != 1 || a.getWindow() != newWindow {
		t.Fatal("旧窗口迟到事件影响了新窗口")
	}
}

func TestWindowsExplicitDestroyDuringShowStaysHidden(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	oldWindow := &application.WebviewWindow{}
	a.setWindow(oldWindow)
	showEntered := make(chan struct{})
	releaseShow := make(chan struct{})
	showDone := make(chan struct{})
	closed := make(chan struct{})
	var builds atomic.Int32
	a.wakeActivate = func(win *application.WebviewWindow, built, maximised, focus bool) {
		if win == oldWindow {
			close(showEntered)
			<-releaseShow
		}
	}
	a.wakeBuild = func(application.WebviewWindowOptions) *application.WebviewWindow {
		builds.Add(1)
		return &application.WebviewWindow{}
	}
	a.destroySnapshot = func(*application.WebviewWindow) (application.Rect, bool) {
		return application.Rect{Width: 1080, Height: 680}, false
	}
	a.destroyClose = func(*application.WebviewWindow) { close(closed) }
	go func() { a.showFromTrayWindows(false); close(showDone) }()
	waitWakeDone(t, showEntered)
	a.ShowMainWindow() // 此聚焦请求早于用户明确销毁，必须一并取消。
	a.destroyRenderProcessFor(oldWindow)
	select {
	case <-closed:
		t.Fatal("旧窗口的 Show 尚在执行时不应并发 Close")
	default:
	}
	close(releaseShow)
	waitWakeDone(t, showDone)
	waitWakeDone(t, closed)
	waitWakeIdle(t, a)
	if a.getWindow() != nil || builds.Load() != 0 || a.recreating.Load() {
		t.Fatal("显示旧窗后用户明确销毁，最终应保持无窗口")
	}
}

func TestWindowsQuitWhileBuildInFlightClosesUnpublishedWindow(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	newWindow := &application.WebviewWindow{}
	enteredBuild := make(chan struct{})
	releaseBuild := make(chan struct{})
	done := make(chan struct{})
	var closeFlag atomic.Bool
	var closes, activations, wakes atomic.Int32
	a.wakeBuild = func(application.WebviewWindowOptions) *application.WebviewWindow {
		a.winMu.Lock()
		a.windowCloseFlags = map[*application.WebviewWindow]*atomic.Bool{newWindow: &closeFlag}
		a.winMu.Unlock()
		close(enteredBuild)
		<-releaseBuild
		return newWindow
	}
	a.destroyClose = func(win *application.WebviewWindow) {
		if win != newWindow || !closeFlag.Load() {
			t.Errorf("未以程序化关闭标志清理新建的隐藏窗口")
		}
		closes.Add(1)
	}
	a.wakeActivate = func(*application.WebviewWindow, bool, bool, bool) { activations.Add(1) }
	a.emitHook = func(name string, _ any) {
		if name == "sys:wake" {
			wakes.Add(1)
		}
	}
	go func() { a.showFromTrayWindows(false); close(done) }()
	waitWakeDone(t, enteredBuild)
	a.QuitApp()
	close(releaseBuild)
	waitWakeDone(t, done)
	if a.getWindow() != nil || closes.Load() != 1 || activations.Load() != 0 || wakes.Load() != 0 {
		t.Fatalf("退出中建窗须关闭且不得发布/显示/发唤醒事件: closes=%d activations=%d wakes=%d", closes.Load(), activations.Load(), wakes.Load())
	}
	a.winMu.Lock()
	_, retained := a.windowCloseFlags[newWindow]
	a.winMu.Unlock()
	if retained {
		t.Fatal("已清理窗口的关闭标志仍留在表中")
	}
}

func TestWindowsQuitCancelsPendingWake(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	oldWindow := &application.WebviewWindow{}
	a.setWindow(oldWindow)
	enteredClose := make(chan struct{})
	releaseClose := make(chan struct{})
	destroyed := make(chan struct{})
	var builds atomic.Int32
	a.destroySnapshot = func(*application.WebviewWindow) (application.Rect, bool) {
		return application.Rect{Width: 1080, Height: 680}, false
	}
	a.destroyClose = func(*application.WebviewWindow) {
		close(enteredClose)
		<-releaseClose
	}
	a.wakeBuild = func(application.WebviewWindowOptions) *application.WebviewWindow {
		builds.Add(1)
		return &application.WebviewWindow{}
	}
	a.wakeActivate = func(*application.WebviewWindow, bool, bool, bool) {}
	go func() { a.destroyRenderProcessFor(oldWindow); close(destroyed) }()
	waitWakeDone(t, enteredClose)
	a.showFromTrayWindows(false)
	a.QuitApp()
	close(releaseClose)
	waitWakeDone(t, destroyed)
	waitWakeIdle(t, a)
	a.showFromTrayWindows(true)
	if builds.Load() != 0 || a.getWindow() != nil || a.recreating.Load() {
		t.Fatal("退出期间排队或后到的唤醒不得重建窗口")
	}
}

func TestWindowsServiceShutdownReleasesWindowCloseFlags(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	a.registerWindowCloseFlag(&application.WebviewWindow{}, &atomic.Bool{})
	a.registerWindowCloseFlag(&application.WebviewWindow{}, &atomic.Bool{})
	a.QuitApp()
	if err := a.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	a.winMu.Lock()
	defer a.winMu.Unlock()
	if a.windowCloseFlags != nil {
		t.Fatalf("退出后应释放全部窗口关闭标志，剩余 %d 条", len(a.windowCloseFlags))
	}
}

func TestWindowsServiceShutdownDoesNotReviveWindowCloseFlagsDuringBuild(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	win := &application.WebviewWindow{}
	enteredBuild := make(chan struct{})
	releaseBuild := make(chan struct{})
	done := make(chan struct{})
	var closeFlag atomic.Bool
	var closes, activations atomic.Int32
	a.wakeBuild = func(application.WebviewWindowOptions) *application.WebviewWindow {
		close(enteredBuild)
		<-releaseBuild
		a.registerWindowCloseFlag(win, &closeFlag)
		return win
	}
	a.destroyClose = func(target *application.WebviewWindow) {
		if target != win {
			t.Errorf("退出时关闭了非本次建造的窗口")
		}
		closes.Add(1)
	}
	a.wakeActivate = func(*application.WebviewWindow, bool, bool, bool) { activations.Add(1) }
	go func() { a.showFromTrayWindows(false); close(done) }()
	waitWakeDone(t, enteredBuild)
	if err := a.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	close(releaseBuild)
	waitWakeDone(t, done)
	a.showFromTrayWindows(true)
	a.winMu.Lock()
	defer a.winMu.Unlock()
	if a.windowCloseFlags != nil || a.window != nil || closes.Load() != 1 || activations.Load() != 0 {
		t.Fatalf("退出后的在途建窗不得重新登记或显示: flags=%d closes=%d activations=%d", len(a.windowCloseFlags), closes.Load(), activations.Load())
	}
}
