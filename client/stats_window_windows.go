//go:build windows

package main

// Windows destroy 模式没有主 WebView，语音统计仅供该页面消费。
func (a *App) shouldCollectVoiceStats() bool { return a.getWindow() != nil }
