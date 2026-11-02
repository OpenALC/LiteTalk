package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestACMEHealthCovered LITETALK_ADDR 已覆盖 8077 时不再额外起回环 healthz 监听（#102）：
// 主 mux 本身就在该端口提供 /api/v1/healthz，再绑一次必然冲突。
func TestACMEHealthCovered(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":443", false},            // ACME 默认：需要单独的 loopback 8077
		{"0.0.0.0:443", false},     //
		{"127.0.0.1:8077", true},   // 显式配成回环 8077：主 mux 已覆盖
		{":8077", true},            // 通配 8077：同样已覆盖
		{"0.0.0.0:8077", true},     //
		{"127.0.0.1:18077", false}, // 自定义端口不覆盖
		{"bad-addr", false},        // 解析失败：照常尝试（起不来只告警）
		{"", false},                //
	}
	for _, tc := range cases {
		if got := acmeHealthCovered(tc.addr); got != tc.want {
			t.Fatalf("acmeHealthCovered(%q) = %v，want %v", tc.addr, got, tc.want)
		}
	}
}

// TestListenOptionalBindFailureIsNotFatal 回环 healthz 端口被占用时只返回错误，
// 不触发 stop（主服务继续跑）。用真实监听占用端口来构造冲突。
func TestListenOptionalBindFailureIsNotFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	srv := &http.Server{Addr: addr, Handler: http.NotFoundHandler()}
	if err := listenOptional("test health", srv); err == nil {
		t.Fatal("端口被占用时应返回错误")
	}
	// 主服务不受影响：同一进程里再监听别的端口仍然成功
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("主服务监听不应受影响: %v", err)
	}
	ln2.Close()
}

// TestListenOptionalServesWhenFree 空闲端口上正常起监听，healthz 可达。
func TestListenOptionalServesWhenFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // 释放后让 listenOptional 自己绑

	srv := &http.Server{Addr: addr, Handler: healthOnlyHandler("v0.0.1-test")}
	if err := listenOptional("test health", srv); err != nil {
		t.Fatalf("空闲端口应可监听: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get("http://" + addr + "/api/v1/healthz")
	if err != nil {
		t.Fatalf("healthz 不可达: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "v0.0.1-test") {
		t.Fatalf("healthz = %d %s", resp.StatusCode, body)
	}
}
