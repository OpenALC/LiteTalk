package desktop

import "github.com/OpenALC/LiteTalk/client/core/hotkey"

// HotkeyEffect 是一次热键事件要产生的效果。
type HotkeyEffect struct {
	PTTPress       bool // 按住说话：打开发送门
	PTTRelease     bool // 松开说话：关门并开尾包窗
	WhisperPress   bool // 按住悄悄话：发送目标集合并开旁路门（#45）
	WhisperRelease bool // 松开悄悄话：清目标并关旁路门
	Mute           bool // 切换闭麦
	Deafen         bool // 切换静音（耳聋）
	Dual           bool // 全关 / 全开
}

// EffectOf 把一次热键事件翻译成要做的动作。
//
// 两类动作的触发沿完全不同，混用会出线上事故：
//   - PTT 是「按住说话」，按下和抬起都要响应，少一个就变成开关或卡在说话；
//   - 闭麦 / 静音 / 全关全是锁存开关，用户心理模型是「按一下切一次」，
//     **只能由按下沿触发**。若抬起也响应，实际行为变成「按住闭麦、松开
//     恢复说话」——与任何用户对快捷键的预期都相反。
func EffectOf(ev hotkey.Event) HotkeyEffect {
	var e HotkeyEffect
	switch ev.Action {
	case hotkey.ActionPTT:
		if ev.Pressed {
			e.PTTPress = true
		} else {
			e.PTTRelease = true
		}
	case hotkey.ActionWhisper:
		// 悄悄话与 PTT 同为「按住」语义：按下/抬起都要响应，否则会卡在开麦
		// 或永远发不出去（#45）。
		if ev.Pressed {
			e.WhisperPress = true
		} else {
			e.WhisperRelease = true
		}
	case hotkey.ActionMute, hotkey.ActionDeafen, hotkey.ActionDual:
		if !ev.Pressed {
			return e // 锁存开关忽略抬起沿
		}
		switch ev.Action {
		case hotkey.ActionMute:
			e.Mute = true
		case hotkey.ActionDeafen:
			e.Deafen = true
		case hotkey.ActionDual:
			e.Dual = true
		}
	}
	return e
}
