package main

import (
	"fmt"
	"os"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/OpenALC/LiteTalk/client/core/voice"
)

// authedClient 已登录 REST 客户端（缺登录态直接报错）。
func authedClient(addrFlag string) (*api.APIClient, *State, error) {
	st, err := loadState()
	if err != nil {
		return nil, nil, err
	}
	if st.AccessToken == "" {
		return nil, nil, fmt.Errorf("未登录（先 litetalk-cli login）")
	}
	base := resolveBase(addrFlag, st)
	return newAPI(base, st), st, nil
}

// room 一次房间命令的运行态：REST 客户端 + 信令会话 + 事件汇聚。
type room struct {
	api  *api.APIClient
	st   *State
	sess *voice.Session
	ev   *console
	base string
}

// newVoiceSession 构造 CLI 会话。CLI 跑在无界面、容器、CI 等场合，固定选择
// BackendNull：不打开真实声卡，由 10ms 节拍器驱动合成信号走完
// DSP→Opus→RTP→混音全链路（上行是 440Hz 测试音，不做真实采集）。
// 桌面端不经过这里，仍由 app.go 走默认的 BackendDevice。
// SetAudioBackend 必须在 Connect 之前——openRoom 在 Connect 前调用本函数。
func newVoiceSession(c *api.APIClient, st *State, ev *console) (*voice.Session, error) {
	sess := voice.NewSession(c, st.AccessToken, st.Nickname, ev.events(), ev.logger())
	if err := sess.SetAudioBackend(audio.BackendNull); err != nil {
		return nil, err
	}
	return sess, nil
}

// openRoom 建信令会话并等到连上。音频管线随 Connect 装配：构造失败降级
// （voiceUnavailable，信令照常），启动失败（无声卡）由 mediaSpy 标记——
// 与 Session 语义一致：语音不可用不阻断登录/入房/聊天。
func openRoom(addrFlag string) (*room, error) {
	c, st, err := authedClient(addrFlag)
	if err != nil {
		return nil, err
	}
	base := resolveBase(addrFlag, st)
	wsURL, err := wsURL(base)
	if err != nil {
		return nil, err
	}
	ev := newConsole()
	sess, err := newVoiceSession(c, st, ev)
	if err != nil {
		return nil, err
	}
	// ChatHandler/ErrorHandler 依赖 ws 实例（ws 为 nil 时静默丢弃），
	// 必须在 Connect 之后挂——app.go 同序。
	sess.Connect(wsURL)
	sess.ChatHandler(func(channelID, msgID, userID, nickname, content, ts string) {
		m := chatMsg{channelID, msgID, userID, nickname, content, ts}
		fmt.Printf("event chat channel=%s id=%s from=%s nick=%s content=%s\n",
			channelID, msgID, userID, nickname, content)
		select {
		case ev.chatCh <- m:
		default:
		}
	})
	sess.ErrorHandler(func(code, message string) {
		fmt.Printf("event error code=%s msg=%s\n", code, message)
	})
	deadline := time.Now().Add(10 * time.Second)
	for !sess.SignalConnected() {
		if time.Now().After(deadline) {
			sess.Close()
			return nil, fmt.Errorf("信令连接超时: %s", wsURL)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("event connected ws=%s proto=v%d\n", wsURL, sess.ProtocolVersion())
	return &room{api: c, st: st, sess: sess, ev: ev, base: base}, nil
}

// close 拆会话。
func (r *room) close() {
	if r.sess != nil {
		r.sess.Close()
	}
}

// wsURL 信令地址：LITETALK_WS 显式覆盖 > 由 REST 基址推导（config.DeriveWS）。
func wsURL(base string) (string, error) {
	if v := os.Getenv("LITETALK_WS"); v != "" {
		return v, nil
	}
	return config.DeriveWS(base)
}

// awaitJoin 等 channel.joined（以 OnSelf 回调为完成信号）。
func (r *room) awaitJoin(timeout time.Duration) (string, error) {
	select {
	case uid := <-r.ev.joinedCh:
		return uid, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("入房超时（未收到 channel.joined）")
	}
}

// printVoiceState 入房后打印成员视图与媒体降级状态。
func (r *room) printVoiceState() {
	vs := r.sess.VoiceState()
	fmt.Printf("room channel=%s members=%d muted=%v deafened=%v\n",
		vs.ChannelID, len(vs.Members), vs.Muted, vs.Deafened)
	for _, m := range vs.Members {
		fmt.Printf("member uid=%s nick=%s tag=%s speaking=%v mute=%v deafen=%v\n",
			m.UserID, m.Nickname, m.Tag, m.Speaking, m.Mute, m.Deafen)
	}
	if d := r.ev.mediaDegraded(); d != "" {
		fmt.Printf("voice degraded: %s（信令照常，语音禁用）\n", d)
	} else {
		fmt.Println("voice media: pipeline ok")
	}
}
