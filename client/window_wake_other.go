//go:build !windows

package main

import "github.com/wailsapp/wails/v3/pkg/application"

func (a *App) isCurrentWindow(target *application.WebviewWindow) bool { return a.getWindow() == target }

func (a *App) interceptCloseForWindow(*application.WebviewWindow) bool { return false }

func (a *App) showFromTrayWindows(bool) {}

func (a *App) resumePendingWakeWindows() {}
