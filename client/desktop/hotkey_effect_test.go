package desktop

import (
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/hotkey"
)

// #45：悄悄话是按住语义，按下/抬起都要响应；锁存开关仍只认按下沿。
func TestEffectOfWhisper(t *testing.T) {
	if e := EffectOf(hotkey.Event{Action: hotkey.ActionWhisper, Pressed: true}); !e.WhisperPress || e.WhisperRelease {
		t.Fatalf("悄悄话按下应只产生 WhisperPress: %+v", e)
	}
	if e := EffectOf(hotkey.Event{Action: hotkey.ActionWhisper, Pressed: false}); !e.WhisperRelease || e.WhisperPress {
		t.Fatalf("悄悄话抬起应只产生 WhisperRelease: %+v", e)
	}
	// 抬起沿不得触发锁存开关。
	for _, a := range []hotkey.Action{hotkey.ActionMute, hotkey.ActionDeafen, hotkey.ActionDual} {
		if e := EffectOf(hotkey.Event{Action: a, Pressed: false}); e.Mute || e.Deafen || e.Dual {
			t.Fatalf("%s 抬起不应触发锁存动作: %+v", a, e)
		}
	}
}
