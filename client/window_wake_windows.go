package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
)

// isCurrentWindow 让迟到的旧窗口事件只作用于它自己，不能转而操作新主窗。
func (a *App) isCurrentWindow(target *application.WebviewWindow) bool {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	return target != nil && a.window == target && a.destroyingWindow != target
}

func (a *App) interceptCloseForWindow(target *application.WebviewWindow) bool {
	if a.forceQuit.Load() || !a.isCurrentWindow(target) {
		return false
	}
	switch closeActionFor(a.cfg.HibernateMode(), a.trayReady.Load(), a.cfg.CloseToTray()) {
	case closeActionDestroy:
		a.destroyRenderProcessFor(target)
		return true
	case closeActionMinimise:
		target.Minimise()
		return true
	case closeActionTray:
		target.Hide()
		a.emitHibernate()
		return true
	default:
		if a.isCurrentWindow(target) {
			a.quitAfterClose()
		}
		return false
	}
}

// showFromTrayWindows 在锁内认领一次唤醒，建窗、显示、聚焦均在锁外。
// 并发普通唤醒由首个调用完成；单实例聚焦请求或销毁中请求延后重放。
func (a *App) showFromTrayWindows(focus bool) {
	a.winMu.Lock()
	if a.destroyingWindow != nil {
		a.wakePending = true
		a.wakePendingFocus = a.wakePendingFocus || focus
		a.winMu.Unlock()
		return
	}
	if a.wakeInProgress {
		if focus {
			a.wakePending = true
			a.wakePendingFocus = true
		}
		a.winMu.Unlock()
		return
	}
	a.wakeInProgress = true
	a.wakeDone = make(chan struct{})
	a.winMu.Unlock()
	a.runWakeWindows(focus)
}

func (a *App) runWakeWindows(focus bool) {
	defer a.finishWakeWindows()
	a.winMu.Lock()
	win := a.window
	var bounds application.Rect
	if a.savedBounds != nil {
		bounds = *a.savedBounds
	}
	maximised := a.savedMaximised
	a.winMu.Unlock()

	built := win == nil
	if built {
		if a.wails == nil && a.wakeBuild == nil {
			return
		}
		opts := application.WebviewWindowOptions{
			Title:            "轻音 LiteTalk",
			Width:            1080,
			Height:           680,
			MinWidth:         960,
			MinHeight:        600,
			Frameless:        true,
			BackgroundColour: application.NewRGB(255, 255, 255),
			Hidden:           true,
		}
		if bounds.Width > 0 {
			opts.X, opts.Y = bounds.X, bounds.Y
			opts.Width, opts.Height = bounds.Width, bounds.Height
		}
		if a.wakeBuild != nil {
			win = a.wakeBuild(opts)
		} else {
			win = a.buildWindow(opts)
		}
		if win == nil {
			return
		}
		a.winMu.Lock()
		a.window = win
		a.winMu.Unlock()
		a.resumeLevelMonitor(win)
	}
	if !a.isCurrentWindow(win) {
		return
	}
	if a.wakeActivate != nil {
		a.wakeActivate(win, built, maximised, focus)
	} else {
		if built && maximised {
			win.Maximise()
		}
		win.Show()
		if win.IsMinimised() {
			win.Restore()
		}
		if focus {
			win.Focus()
		}
	}
	if a.isCurrentWindow(win) {
		a.emit("sys:wake", nil)
	}
}

func (a *App) finishWakeWindows() {
	a.winMu.Lock()
	a.wakeInProgress = false
	close(a.wakeDone)
	a.wakeDone = nil
	a.winMu.Unlock()
	a.resumePendingWakeWindows()
}

func (a *App) resumePendingWakeWindows() {
	a.winMu.Lock()
	if !a.wakePending || a.wakeInProgress || a.destroyingWindow != nil {
		a.winMu.Unlock()
		return
	}
	a.wakeInProgress = true
	a.wakeDone = make(chan struct{})
	a.wakePending = false
	focus := a.wakePendingFocus
	a.wakePendingFocus = false
	a.winMu.Unlock()
	go a.runWakeWindows(focus)
}
