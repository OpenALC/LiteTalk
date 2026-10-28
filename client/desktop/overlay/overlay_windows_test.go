//go:build windows

package overlay

import "testing"

func TestStatusText(t *testing.T) {
	cases := []struct {
		sp   Speaker
		want string
	}{
		{Speaker{}, ""},
		{Speaker{Mute: true}, "闭麦"},
		{Speaker{Deafen: true}, "静音"},
		{Speaker{Priority: true}, "优先"},
		{Speaker{Mute: true, Deafen: true}, "闭麦 · 静音"},
		{Speaker{Mute: true, Deafen: true, Priority: true}, "闭麦 · 静音 · 优先"},
	}
	for _, c := range cases {
		if got := statusText(c.sp); got != c.want {
			t.Errorf("statusText(%+v) = %q, want %q", c.sp, got, c.want)
		}
	}
}
