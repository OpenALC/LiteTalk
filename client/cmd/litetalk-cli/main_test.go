package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
)

// TestNewVoiceSessionUsesNullBackend CLI 构造路径固定走 BackendNull（#79）：
// 无界面/容器/CI 环境没有可用的声卡，必须能在装配阶段就把后端定下来。
func TestNewVoiceSessionUsesNullBackend(t *testing.T) {
	st := &State{AccessToken: "tok", Nickname: "甲"}
	sess, err := newVoiceSession(api.NewAPIClient("http://127.0.0.1:1"), st, newConsole())
	if err != nil {
		t.Fatalf("newVoiceSession: %v", err)
	}
	defer sess.Close()
	if got := sess.AudioBackend(); got != audio.BackendNull {
		t.Fatalf("CLI 音频后端 = %v, want BackendNull", got)
	}
}

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

// TestConsoleVoiceUnavailableRecords OnVoiceUnavailable 是 CLI 观测媒体不可用的
// 正规通道（#80）：初始化失败与入房后启动失败都走这里，printVoiceState 据此明示。
func TestConsoleVoiceUnavailableRecords(t *testing.T) {
	c := newConsole()
	c.events().OnVoiceUnavailable("音频设备启动失败，请检查麦克风和扬声器")
	if got := c.mediaDegraded(); got != "音频设备启动失败，请检查麦克风和扬声器" {
		t.Fatalf("mediaDegraded = %q", got)
	}
}

// TestWaitChatEcho send 的 WS 回显判定：跳过其它频道/其它内容的事件，
// 匹配到本人消息才成功；完全没回显时超时报错（无 REST 兜底）。
func TestWaitChatEcho(t *testing.T) {
	ch := make(chan chatMsg, 4)
	ch <- chatMsg{channelID: "9", content: "other", msgID: "m0"} // 别的频道
	ch <- chatMsg{channelID: "1", content: "别的消息", msgID: "m1"}
	ch <- chatMsg{channelID: "1", content: "hello", msgID: "m2"}
	got, err := waitChatEcho(ch, "1", "hello", time.Second)
	if err != nil || got.msgID != "m2" {
		t.Fatalf("回显匹配 = (%+v,%v), want m2", got, err)
	}

	empty := make(chan chatMsg)
	if _, err := waitChatEcho(empty, "1", "hello", 20*time.Millisecond); err == nil {
		t.Fatal("无回显应超时报错")
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
