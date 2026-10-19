package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/OpenALC/LiteTalk/client/core/voice"
)

// chatMsg 聊天回调载荷（ChatHandler 签名是六个裸参数，这里聚合成结构体传递）。
type chatMsg struct {
	channelID, msgID, userID, nickname, content, ts string
}

// console 把 voice.Events 落到标准输出，兼作房间命令的等待原语
// （join 等 OnSelf、send 等 chat 回显）。媒体降级经 noteMedia 标记——
// core 未暴露媒体启动结果，见 mediaSpy 注释。
type console struct {
	mu           sync.Mutex
	mediaErr     string // 非空 = 媒体管线降级（voiceUnavailable 或启动失败）
	joinedCh     chan string
	chatCh       chan chatMsg
	speakingFlip int
}

func newConsole() *console {
	return &console{
		joinedCh: make(chan string, 4),
		chatCh:   make(chan chatMsg, 16),
	}
}

// noteMedia 记录媒体降级原因（log spy 与 OnVoiceUnavailable 共用）。
func (c *console) noteMedia(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mediaErr == "" {
		c.mediaErr = msg
	}
}

// mediaDegraded 返回降级原因（未降级为空串）。
func (c *console) mediaDegraded() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mediaErr
}

// events 组装会话事件回调：全部只做打印/投递，不阻塞 ws 读循环。
func (c *console) events() voice.Events {
	return voice.Events{
		OnConnection: func(state string) {
			fmt.Printf("event connection state=%s\n", state)
		},
		OnSelf: func(userID string) {
			fmt.Printf("event self id=%s\n", userID)
			select {
			case c.joinedCh <- userID:
			default:
			}
		},
		OnPeerJoin: func(userID, nickname, tag string, speaking, mute, deafen bool) {
			fmt.Printf("event peer-join uid=%s nick=%s tag=%s speaking=%v mute=%v deafen=%v\n",
				userID, nickname, tag, speaking, mute, deafen)
		},
		OnPeerLeave: func(userID string) {
			fmt.Printf("event peer-leave uid=%s\n", userID)
		},
		OnPeerSpeaking: func(userID string, speaking, mute, deafen bool) {
			c.mu.Lock()
			c.speakingFlip++
			n := c.speakingFlip
			c.mu.Unlock()
			fmt.Printf("event speaking uid=%s speaking=%v mute=%v deafen=%v n=%d\n",
				userID, speaking, mute, deafen, n)
		},
		OnVoiceUnavailable: func(errMsg string) {
			c.noteMedia("voiceUnavailable: " + errMsg)
			fmt.Printf("event voice-unavailable err=%s\n", errMsg)
		},
		OnKicked: func(reason string) {
			fmt.Printf("event kicked reason=%s\n", reason)
		},
		OnVoiceTaken: func() {
			fmt.Println("event voice-taken")
		},
		OnPresenceUpdate: func(userID string, online bool) {
			fmt.Printf("event presence uid=%s online=%v\n", userID, online)
		},
		OnServerShutdown: func(reason string) {
			fmt.Printf("event server-shutdown reason=%s\n", reason)
		},
		OnRTCFailed: func(channelID string) {
			fmt.Printf("event rtc-failed channel=%s\n", channelID)
		},
		OnStateRequest: func(fromID, fromNickname string, mute, deafen *bool) {
			fmt.Printf("event state-request from=%s nick=%s\n", fromID, fromNickname)
		},
		OnResync: func() {
			fmt.Println("event resync")
		},
	}
}

// mediaSpy 会话日志旁路：识别「媒体管线启动失败」这一降级路径。
// voiceUnavailable 只覆盖管线构造失败（如缺 Opus）；无声卡/无设备时构造成功、
// startMedia 启动失败，Session 仅打日志不给事件——消费方拿不到结果（core 缺口，
// 见 #34 汇报）。这里拦截已知日志行做明示，属权宜之计。
type mediaSpy struct {
	c *console
	w io.Writer
}

func (s *mediaSpy) Write(p []byte) (int, error) {
	line := string(p)
	if strings.Contains(line, "pipeline start:") ||
		strings.Contains(line, "mixer start:") ||
		strings.Contains(line, "pipeline unavailable:") {
		s.c.noteMedia(strings.TrimSpace(line))
	}
	return s.w.Write(p)
}

// logger 会话日志器：转发 stderr 的同时喂 mediaSpy。
func (c *console) logger() *log.Logger {
	return log.New(&mediaSpy{c: c, w: os.Stderr}, "voice: ", log.Ltime)
}
