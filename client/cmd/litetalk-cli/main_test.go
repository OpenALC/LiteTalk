package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResolveBasePriority flag 显式 > LITETALK_API > 状态文件 > 默认。
func TestResolveBasePriority(t *testing.T) {
	t.Setenv("LITETALK_API", "")
	if got := resolveBase("http://flag.example", &State{BaseURL: "http://file.example"}); got != "http://flag.example" {
		t.Fatalf("flag 应最高优先, 得到 %q", got)
	}
	t.Setenv("LITETALK_API", "http://env.example/")
	if got := resolveBase("", &State{BaseURL: "http://file.example"}); got != "http://env.example" {
		t.Fatalf("env 应高于文件且去尾斜杠, 得到 %q", got)
	}
	t.Setenv("LITETALK_API", "")
	if got := resolveBase("", &State{BaseURL: "http://file.example"}); got != "http://file.example" {
		t.Fatalf("env 空时应回落文件, 得到 %q", got)
	}
	if got := resolveBase("", &State{}); got != "http://127.0.0.1:8077" {
		t.Fatalf("无覆盖应回落默认本机, 得到 %q", got)
	}
}

// TestStateRoundTrip 落盘/读回：0600 令牌文件能还原字段。
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// UserConfigDir 不可注入，直接改 HOME/APPDATA 之外用 statePath 的并行路径验证
	// 编解码：save/load 在固定临时文件上做等价往返。
	p := filepath.Join(dir, "state.json")
	st := &State{
		BaseURL:      "http://127.0.0.1:8077",
		AccessToken:  "acc-1",
		RefreshToken: "ref-1",
		UserID:       "7",
		Nickname:     "甲",
		Tag:          "1234",
	}
	b, err := marshalState(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readStateFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *st {
		t.Fatalf("往返不等: %+v != %+v", *got, *st)
	}
	// 权限位是 Unix 语义；Windows 由 ACL 表达，0600 传给 WriteFile 只作创建属性，
	// 这里不作断言（非功能缺失，是文件系统语义差异）。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("令牌文件应 0600, 得到 %o", perm)
		}
	}
}

// TestMediaSpyDetectsPipelineFailure 识别 Session 日志里的媒体启动失败行。
func TestMediaSpyDetectsPipelineFailure(t *testing.T) {
	c := newConsole()
	spy := &mediaSpy{c: c, w: io.Discard}
	for _, line := range []string{
		"voice: pipeline start: no suitable device",
		"voice: mixer start: device busy",
		"voice: pipeline unavailable: init dsp: boom",
	} {
		if _, err := spy.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		if c.mediaDegraded() == "" {
			t.Fatalf("应识别降级行: %q", line)
		}
	}
	// 普通日志行不得误标。
	c2 := newConsole()
	spy2 := &mediaSpy{c: c2, w: io.Discard}
	if _, err := spy2.Write([]byte("voice: signal: 协商协议版本 v2\n")); err != nil {
		t.Fatal(err)
	}
	if got := c2.mediaDegraded(); got != "" {
		t.Fatalf("普通行不应标记降级, 得到 %q", got)
	}
}

// TestNoteMediaKeepsFirst 先到的降级原因为准（后续日志只作补充噪音）。
func TestNoteMediaKeepsFirst(t *testing.T) {
	c := newConsole()
	c.noteMedia("first")
	c.noteMedia("second")
	if got := c.mediaDegraded(); !strings.HasPrefix(got, "first") {
		t.Fatalf("应保留首个原因, 得到 %q", got)
	}
}
