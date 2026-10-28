package overlay

import (
	"errors"
	"testing"
)

// TestIdentityColorIndex 与前端 SoundOrb.vue 的 hashId 对齐：期望值由
// Node 按 `h=(h*31+charCodeAt(i))|0; Math.abs(h)%4` 逐字计算得出。
// 含 UTF-16 代理对（emoji）用例，确认 Go 侧按码元而非码点迭代。
func TestIdentityColorIndex(t *testing.T) {
	cases := map[string]int{
		"1":                  1,
		"2":                  2,
		"10":                 3,
		"123456789012345678": 1,
		"9007199254740993":   2,
		"-1":                 0,
		"abc":                2,
		"轻音":                 0,
		"uid-😀":              0,
	}
	for uid, want := range cases {
		if got := IdentityColorIndex(uid); got != want {
			t.Errorf("IdentityColorIndex(%q) = %d, want %d", uid, got, want)
		}
	}
}

// TestIdentityColorPalette 身份色必须与主题四色中的品牌邻近色一致。
func TestIdentityColorPalette(t *testing.T) {
	want := [4]uint32{0x00F3A44F, 0x00C4BB3D, 0x00F88C7C, 0x007FB53F}
	if identityColors != want {
		t.Fatalf("identityColors = %#v, want %#v", identityColors, want)
	}
}

func TestCompose(t *testing.T) {
	in := []Speaker{
		{UID: "u1", Nickname: "Alice", Speaking: true, Priority: true},
		{UID: "u2", Nickname: "Bob", Speaking: false},
		{UID: "u3", Nickname: "张三", Speaking: true},
		{UID: "u4", Speaking: true}, // 昵称为空回落到 UID
		{UID: "u5", Nickname: "Carol", Speaking: true},
		{UID: "u6", Nickname: "Dave", Speaking: true},
		{UID: "u7", Nickname: "Eve", Speaking: true},
		{UID: "u8", Nickname: "Frank", Speaking: true},
		{UID: "u1", Nickname: "Alice", Speaking: true, Priority: true}, // 重复 UID
		{Speaking: true}, // UID 与昵称都空，丢弃
	}
	got := compose(in)
	want := []string{"Alice", "Carol", "Dave", "Eve", "Frank", "u4"}
	if len(got) != len(want) {
		t.Fatalf("compose 长度 = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Nickname != want[i] {
			t.Fatalf("compose[%d] = %q, want %q（全量 %+v）", i, got[i].Nickname, want[i], got)
		}
	}
}

func TestComposePriorityBeforeNickname(t *testing.T) {
	in := []Speaker{
		{UID: "a", Nickname: "Alice", Speaking: true},
		{UID: "z", Nickname: "Zoe", Speaking: true, Priority: true},
		{UID: "b", Nickname: "Bob", Speaking: true, Priority: true},
	}
	got := compose(in)
	want := []string{"Bob", "Zoe", "Alice"}
	for i := range want {
		if got[i].Nickname != want[i] {
			t.Fatalf("compose[%d] = %q, want %q", i, got[i].Nickname, want[i])
		}
	}
}

func TestComposeEmpty(t *testing.T) {
	if got := compose(nil); len(got) != 0 {
		t.Fatalf("compose(nil) = %+v", got)
	}
	if got := compose([]Speaker{{UID: "u", Nickname: "n"}}); len(got) != 0 {
		t.Fatalf("非说话者不应展示: %+v", got)
	}
}

func TestSameSpeakers(t *testing.T) {
	a := []Speaker{{UID: "1", Nickname: "A", Speaking: true}}
	if !sameSpeakers(a, a) {
		t.Fatal("同一份快照应判定相等")
	}
	if sameSpeakers(a, nil) {
		t.Fatal("长度不同应判定不等")
	}
	b := []Speaker{{UID: "1", Nickname: "A", Speaking: true, Mute: true}}
	if sameSpeakers(a, b) {
		t.Fatal("状态位变化应判定不等")
	}
}

// fakePlatform 是生命周期测试用的平台桩：不创建真实窗口。
type fakePlatform struct {
	supported bool
	startErr  error
	starts    int
	stops     int
	snapshot  Snapshot
}

func withFake(t *testing.T, f *fakePlatform) {
	t.Helper()
	oldSupported, oldStart, oldStop := platformSupported, platformStart, platformStop
	platformSupported = func() bool { return f.supported }
	platformStart = func(s Snapshot) error {
		f.starts++
		f.snapshot = s
		return f.startErr
	}
	platformStop = func() { f.stops++ }
	t.Cleanup(func() {
		platformSupported, platformStart, platformStop = oldSupported, oldStart, oldStop
		mu.Lock()
		started = false
		mu.Unlock()
	})
}

func TestStartStopIdempotent(t *testing.T) {
	f := &fakePlatform{supported: true}
	withFake(t, f)
	snap := func() []Speaker { return nil }

	if !Supported() {
		t.Fatal("桩平台应报告支持")
	}
	if err := Start(snap); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := Start(snap); err != nil {
		t.Fatalf("重复 Start 应无副作用: %v", err)
	}
	if f.starts != 1 {
		t.Fatalf("platformStart 调用 %d 次，want 1", f.starts)
	}
	if f.snapshot == nil {
		t.Fatal("snapshot 未传给平台实现")
	}
	Stop()
	Stop()
	if f.stops != 1 {
		t.Fatalf("platformStop 调用 %d 次，want 1", f.stops)
	}
	// Stop 后可重新启动（设置页热切换）。
	if err := Start(snap); err != nil {
		t.Fatalf("重新 Start: %v", err)
	}
	if f.starts != 2 {
		t.Fatalf("重新启动后 platformStart 调用 %d 次，want 2", f.starts)
	}
	Stop()
}

func TestStartUnsupported(t *testing.T) {
	f := &fakePlatform{supported: false}
	withFake(t, f)
	if Supported() {
		t.Fatal("桩平台应报告不支持")
	}
	err := Start(func() []Speaker { return nil })
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Start = %v, want ErrUnsupportedPlatform", err)
	}
	if f.starts != 0 {
		t.Fatal("平台不支持时不应调用 platformStart")
	}
	Stop()
	if f.stops != 0 {
		t.Fatal("未启动时 Stop 不应调用 platformStop")
	}
}

func TestStartErrorDoesNotMarkStarted(t *testing.T) {
	f := &fakePlatform{supported: true, startErr: errors.New("boom")}
	withFake(t, f)
	snap := func() []Speaker { return nil }
	if err := Start(snap); err == nil {
		t.Fatal("平台启动失败应返回错误")
	}
	// 失败不置 started：再次尝试仍会调用平台实现（而不是被幂等短路）。
	f.startErr = nil
	if err := Start(snap); err != nil {
		t.Fatalf("第二次 Start: %v", err)
	}
	if f.starts != 2 {
		t.Fatalf("platformStart 调用 %d 次，want 2", f.starts)
	}
	Stop()
}

func TestStartNilSnapshot(t *testing.T) {
	f := &fakePlatform{supported: true}
	withFake(t, f)
	if err := Start(nil); err == nil {
		t.Fatal("nil snapshot 应报错")
	}
	if f.starts != 0 {
		t.Fatal("nil snapshot 不应调用 platformStart")
	}
}

func TestSafeSnapshotPanic(t *testing.T) {
	got := safeSnapshot(func() []Speaker { panic("boom") })
	if got != nil {
		t.Fatalf("panic 后应返回 nil，得到 %+v", got)
	}
}
