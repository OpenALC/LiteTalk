package main

import (
	"flag"
	"fmt"
	"time"
)

// cmdJoin 信令入房：发 channel.join、等 channel.joined、打印成员，保持后退房。
// 验收重点是信令层进房成功（拿到 channel.joined）。
func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	channel := fs.String("channel", "", "频道 ID")
	duration := fs.Duration("duration", 3*time.Second, "入房保持时长")
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	if *channel == "" {
		return fmt.Errorf("join 需要 --channel")
	}
	r, err := openRoom(*addr)
	if err != nil {
		return err
	}
	defer r.close()
	if err := r.sess.JoinChannel(*channel); err != nil {
		return err
	}
	fmt.Printf("join sent channel=%s\n", *channel)
	if _, err := r.awaitJoin(10 * time.Second); err != nil {
		return err
	}
	r.printVoiceState()
	time.Sleep(*duration)
	if err := r.sess.LeaveChannel(); err != nil {
		return err
	}
	fmt.Println("ok join")
	return nil
}

// cmdSend 发文字消息并等自己收到 chat.message 回显（服务端广播含发送者）。
// 无需入房：频道属服务器成员即可发（chat 权限二选一）。
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	channel := fs.String("channel", "", "频道 ID")
	text := fs.String("text", "", "消息内容")
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	if *channel == "" || *text == "" {
		return fmt.Errorf("send 需要 --channel --text")
	}
	r, err := openRoom(*addr)
	if err != nil {
		return err
	}
	defer r.close()
	if err := r.sess.SendChat(*channel, *text); err != nil {
		return err
	}
	fmt.Printf("send channel=%s text=%s\n", *channel, *text)
	// 只认 WS 回显（chat.message 广播含发送者）：send 的语义就是信令面发送，
	// 回显缺席说明事件面没有正常回流，应当暴露而不是用 REST 落库结果掩盖。
	m, err := waitChatEcho(r.ev.chatCh, *channel, *text, 3*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("ok send echoed id=%s ts=%s\n", m.msgID, m.ts)
	return nil
}

// waitChatEcho 等本人消息的 WS 回显；其他频道的在途消息跳过（同一连接会收
// 全服务器事件）。超时 = 事件面没有回流，由调用方报错。
func waitChatEcho(ch <-chan chatMsg, channelID, text string, timeout time.Duration) (chatMsg, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case m := <-ch:
			if m.channelID == channelID && m.content == text {
				return m, nil
			}
		case <-timer.C:
			return chatMsg{}, fmt.Errorf("send 已发出但 %s 内未收到 chat.message 回显", timeout)
		}
	}
}

// cmdCall 入房通话 N 秒打印统计（信令 + 媒体）。无声卡环境媒体降级明示，
// 信令照常——与 voiceUnavailable 语义一致，不报错退出。
func cmdCall(args []string) error {
	return runStay(args, "call", false)
}

// cmdListen 入房旁听：闭麦后保持 N 秒，只收下行（成员/说话态/消息）。
func cmdListen(args []string) error {
	return runStay(args, "listen", true)
}

// runStay call/listen 共用：入房 → 周期打印语音统计 → 保持 duration → 退房。
// listen 额外闭麦（上行不发），call 全双工。
func runStay(args []string, name string, mute bool) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	channel := fs.String("channel", "", "频道 ID")
	duration := fs.Duration("duration", 10*time.Second, "通话时长")
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	if *channel == "" {
		return fmt.Errorf("%s 需要 --channel", name)
	}
	r, err := openRoom(*addr)
	if err != nil {
		return err
	}
	defer r.close()
	if err := r.sess.JoinChannel(*channel); err != nil {
		return err
	}
	fmt.Printf("%s sent channel=%s duration=%s\n", name, *channel, *duration)
	if _, err := r.awaitJoin(10 * time.Second); err != nil {
		return err
	}
	// 闭麦必须在入房之后：入房前发 voice.state 会被服务端判 not_in_channel。
	if mute {
		r.sess.SetMute(true)
		fmt.Println("listen mode: muted uplink")
	}
	r.printVoiceState()
	// 周期统计：媒体活着时 enc_packets/rtp_sent 随时间增长；无声卡降级时
	// 恒 0，与上面的 degraded 明示互相印证。
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	end := time.After(*duration)
	for {
		select {
		case <-tick.C:
			fmt.Printf("stats %s\n", r.sess.Stats())
		case <-end:
			if err := r.sess.LeaveChannel(); err != nil {
				return err
			}
			fmt.Printf("stats %s\n", r.sess.Stats())
			if d := r.ev.mediaDegraded(); d != "" {
				fmt.Printf("ok %s duration=%s voice=degraded\n", name, *duration)
			} else {
				fmt.Printf("ok %s duration=%s voice=on\n", name, *duration)
			}
			return nil
		}
	}
}
