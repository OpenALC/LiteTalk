package state

import (
	"errors"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/client/internal/signal"
)

// TestJoinChannelRollback 发送失败必须回滚 roomID——roomID 先存是为断线重连
// 恢复，失败路径不复位会让 OnConnect 重发 join 造成「幽灵入房」。
func TestJoinChannelRollback(t *testing.T) {
	s := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()

	// ws 未 Run：Send 返回 ErrNotConnected，模拟发送失败路径。
	if err := s.JoinChannel("room1"); err == nil {
		t.Fatal("未连接时 JoinChannel 应返回错误")
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("发送失败后 roomID 应回滚为空, 得到 %q", got)
	}
}

// TestJoinChannelVoiceUnavailable 媒体管线初始化失败（如无 Opus 的平台）时
// JoinChannel 必须给出明确错误供前端 toast，而不是静默成功或 panic。
func TestJoinChannelVoiceUnavailable(t *testing.T) {
	s := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()
	s.voiceUnavailable = true
	s.voiceErr = errors.New("opus: unsupported platform")

	err := s.JoinChannel("room1")
	if err == nil {
		t.Fatal("voiceUnavailable 时 JoinChannel 应返回错误")
	}
	if !strings.Contains(err.Error(), "语音不可用") {
		t.Fatalf("错误应提示语音不可用, 得到 %v", err)
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("voiceUnavailable 时不应写入 roomID, 得到 %q", got)
	}
}

// TestHandleKickedForwardsReason session.kicked 的上报出口：原因原样透传，
// 未注册回调时静默（Events 零值可安全构造）。
func TestHandleKickedForwardsReason(t *testing.T) {
	got := ""
	s := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n",
		Events{OnKicked: func(reason string) { got = reason }}, nil)
	s.handleKicked("login_elsewhere")
	if got != "login_elsewhere" {
		t.Fatalf("应原样透传被踢原因, 得到 %q", got)
	}

	// 零值 Events：无回调不得 panic。
	quiet := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	quiet.handleKicked("login_elsewhere")
}
