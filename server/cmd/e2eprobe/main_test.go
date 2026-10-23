package main

import "testing"

// TestSchemes URL 拼接与 -tls 切换：http/ws 与 https/wss 两套 scheme 都必须正确。
func TestSchemes(t *testing.T) {
	oldR, oldW := restScheme, wsScheme
	t.Cleanup(func() { restScheme, wsScheme = oldR, oldW })

	setSchemes(false)
	if restScheme != "http" || wsScheme != "ws" {
		t.Fatalf("setSchemes(false) = %q/%q, want http/ws", restScheme, wsScheme)
	}
	if got, want := restURL("127.0.0.1:8077", "/api/v1/healthz"), "http://127.0.0.1:8077/api/v1/healthz"; got != want {
		t.Fatalf("restURL = %q, want %q", got, want)
	}
	if got, want := wsURL("127.0.0.1:8077", "tok"), "ws://127.0.0.1:8077/ws?token=tok"; got != want {
		t.Fatalf("wsURL = %q, want %q", got, want)
	}

	setSchemes(true)
	if restScheme != "https" || wsScheme != "wss" {
		t.Fatalf("setSchemes(true) = %q/%q, want https/wss", restScheme, wsScheme)
	}
	if got, want := restURL("litetalk.example.com", "/api/v1/healthz"), "https://litetalk.example.com/api/v1/healthz"; got != want {
		t.Fatalf("restURL = %q, want %q", got, want)
	}
	if got, want := wsURL("litetalk.example.com", "tok"), "wss://litetalk.example.com/ws?token=tok"; got != want {
		t.Fatalf("wsURL = %q, want %q", got, want)
	}
}
