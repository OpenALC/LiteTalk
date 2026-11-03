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

func TestWindowsWakeOverlapsInFlightShowAndDestroy(t *testing.T) {
	a := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	oldWindow := &application.WebviewWindow{}
	newWindow := &application.WebviewWindow{}
	a.setWindow(oldWindow)
	showEntered := make(chan struct{})
	releaseShow := make(chan struct{})
	showDone := make(chan struct{})
	newShown := make(chan wakeCall, 1)
	closed := make(chan struct{})
	a.wakeActivate = func(win *application.WebviewWindow, built, maximised, focus bool) {
		if win == oldWindow {
			close(showEntered)
			<-releaseShow
			return
		}
		newShown <- wakeCall{win, built, maximised, focus}
	}
	a.wakeBuild = func(application.WebviewWindowOptions) *application.WebviewWindow { return newWindow }
	a.destroySnapshot = func(*application.WebviewWindow) (application.Rect, bool) {
		return application.Rect{Width: 1080, Height: 680}, false
	}
	a.destroyClose = func(*application.WebviewWindow) { close(closed) }
	go func() { a.showFromTrayWindows(false); close(showDone) }()
	waitWakeDone(t, showEntered)
	a.destroyRenderProcessFor(oldWindow)
	select {
	case <-closed:
		t.Fatal("旧窗口的 Show 尚在执行时不应并发 Close")
	default:
	}
	close(releaseShow)
	waitWakeDone(t, showDone)
	waitWakeDone(t, closed)
	call := waitWakeCall(t, newShown)
	if call.window != newWindow || !call.built || a.getWindow() != newWindow {
		t.Fatalf("显示旧窗与销毁交叉后丢失了恢复: %+v", call)
	}
}
