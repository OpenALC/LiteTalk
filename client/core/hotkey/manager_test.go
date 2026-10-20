package hotkey

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSource 是 Manager 测试用的假事件源：记录调用，按脚本推送事件。
type fakeSource struct {
	avail    bool
	startErr error

	mu        sync.Mutex
	starts    int
	stops     int
	updates   []Settings
	lastStart Settings
	ch        chan<- Event
}

func (f *fakeSource) available() bool { return f.avail }

func (f *fakeSource) start(st Settings, ch chan<- Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.lastStart = st
	f.ch = ch
	if f.startErr != nil {
		return f.startErr
	}
	return nil
}

func (f *fakeSource) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
}

func (f *fakeSource) update(st Settings) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, st)
}

// push 模拟钩子回调往事件通道里灌一条事件；返回是否投递成功。
func (f *fakeSource) push(ev Event) bool {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}

func (f *fakeSource) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func (f *fakeSource) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

func (f *fakeSource) lastUpdate() Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updates) == 0 {
		return Settings{}
	}
	return f.updates[len(f.updates)-1]
}

// newTestManager 构造一个注入假源的 Manager。settings 会被 Sanitize 收敛。
func newTestManager(avail bool, s Settings) (*Manager, *fakeSource) {
	src := &fakeSource{avail: avail}
	m := &Manager{src: src, ch: make(chan Event, eventChanCap), settings: s.Sanitize()}
	return m, src
}

func enabledSettings() Settings {
	return Settings{
		Enabled: true,
		Mode:    ModePTT,
		PTT:     "ctrl+shift+m",
		Mute:    "ctrl+alt+m",
		Deafen:  "ctrl+alt+d",
		Dual:    "ctrl+alt+s",
		TailMS:  250,
	}
}

func TestManagerNewSanitizes(t *testing.T) {
	m, _ := newTestManager(true, Settings{Enabled: true, Mode: Mode(9), TailMS: 99999, PTT: "bogus"})
	if m.settings.Mode != ModePTT {
		t.Errorf("Mode = %d，期望 ModePTT", m.settings.Mode)
	}
	if m.settings.TailMS != maxTailMS {
		t.Errorf("TailMS = %d，期望 %d", m.settings.TailMS, maxTailMS)
	}
	if m.settings.PTT != "" {
		t.Errorf("非法绑定应被清空，实际 %q", m.settings.PTT)
	}
	if cap(m.Events()) != eventChanCap {
		t.Errorf("事件通道容量 = %d，期望 %d", cap(m.Events()), eventChanCap)
	}
}

func TestManagerStartStopLifecycle(t *testing.T) {
	m, src := newTestManager(true, enabledSettings())
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	starts, stops := src.counts()
	if starts != 1 || stops != 0 {
		t.Fatalf("Start 后 starts=%d stops=%d，期望 1/0", starts, stops)
	}
	// 重复 Start 是空操作
	if err := m.Start(); err != nil {
		t.Fatalf("重复 Start 失败: %v", err)
	}
	if starts, _ = src.counts(); starts != 1 {
		t.Errorf("重复 Start 又装了一次钩子：starts=%d", starts)
	}
	m.Stop()
	starts, stops = src.counts()
	if starts != 1 || stops != 1 {
		t.Fatalf("Stop 后 starts=%d stops=%d，期望 1/1", starts, stops)
	}
	// 重复 Stop 安全
	m.Stop()
	m.Stop()
	if _, stops = src.counts(); stops != 1 {
		t.Errorf("重复 Stop 重复卸载：stops=%d", stops)
	}
}

func TestManagerStartDisabledInstallsNothing(t *testing.T) {
	m, src := newTestManager(true, DefaultSettings())
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if starts, _ := src.counts(); starts != 0 {
		t.Errorf("默认关闭时不该装钩子：starts=%d", starts)
	}
	m.Stop()
}

func TestManagerStartUnsupportedPlatform(t *testing.T) {
	src := &fakeSource{avail: false, startErr: ErrUnsupportedPlatform}
	m := &Manager{src: src, ch: make(chan Event, eventChanCap), settings: enabledSettings()}
	err := m.Start()
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Start 应返回 ErrUnsupportedPlatform，实际 %v", err)
	}
	if m.Available() {
		t.Error("Available 应报告 false")
	}
}

func TestManagerAvailableDelegates(t *testing.T) {
	m, src := newTestManager(true, DefaultSettings())
	src.avail = false
	if m.Available() {
		t.Error("Available 应透传平台源")
	}
}

func TestManagerSetSettingsRejectsBadBinding(t *testing.T) {
	m, src := newTestManager(true, enabledSettings())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	before := m.settings
	if err := m.SetSettings(Settings{Enabled: true, Mode: ModePTT, PTT: "ctrl+ctrl+m"}); err == nil {
		t.Fatal("非法绑定应报错")
	}
	if m.settings != before {
		t.Errorf("失败时不应改动现有绑定：%+v", m.settings)
	}
	if n := src.updateCount(); n != 0 {
		t.Errorf("失败时不应热更新：updates=%d", n)
	}
	// 合法更新仍然生效
	next := enabledSettings()
	next.PTT = "mouse4"
	if err := m.SetSettings(next); err != nil {
		t.Fatalf("合法更新应通过: %v", err)
	}
	if n := src.updateCount(); n != 1 {
		t.Fatalf("updates = %d，期望 1", n)
	}
	if got := src.lastUpdate(); got.PTT != "mouse4" {
		t.Errorf("热更新的绑定 = %+v", got)
	}
}

func TestManagerSetSettingsNormalizes(t *testing.T) {
	m, src := newTestManager(true, enabledSettings())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	next := enabledSettings()
	next.PTT = "Shift+Ctrl+M"
	next.TailMS = 5000
	if err := m.SetSettings(next); err != nil {
		t.Fatal(err)
	}
	got := src.lastUpdate()
	if got.PTT != "ctrl+shift+m" {
		t.Errorf("热更新前未规范化：%q", got.PTT)
	}
	if got.TailMS != maxTailMS {
		t.Errorf("TailMS = %d，期望 %d", got.TailMS, maxTailMS)
	}
}

func TestManagerSetSettingsToggleEnabled(t *testing.T) {
	// 关 → 开：已 Start 但没装钩子，SetSettings 应补上安装
	m, src := newTestManager(true, DefaultSettings())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSettings(enabledSettings()); err != nil {
		t.Fatal(err)
	}
	if starts, _ := src.counts(); starts != 1 {
		t.Errorf("启用时应装上钩子：starts=%d", starts)
	}

	// 开 → 关：应卸载
	off := enabledSettings()
	off.Enabled = false
	if err := m.SetSettings(off); err != nil {
		t.Fatal(err)
	}
	if _, stops := src.counts(); stops != 1 {
		t.Errorf("关闭时应卸载钩子：stops=%d", stops)
	}

	// 再开：重新装
	if err := m.SetSettings(enabledSettings()); err != nil {
		t.Fatal(err)
	}
	if starts, _ := src.counts(); starts != 2 {
		t.Errorf("再启用时应重新装钩子：starts=%d", starts)
	}
}

func TestManagerSetSettingsBeforeStart(t *testing.T) {
	m, src := newTestManager(true, DefaultSettings())
	if err := m.SetSettings(enabledSettings()); err != nil {
		t.Fatal(err)
	}
	if starts, _ := src.counts(); starts != 0 {
		t.Errorf("未 Start 时不该装钩子：starts=%d", starts)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if starts, _ := src.counts(); starts != 1 {
		t.Errorf("Start 应按最新设置装钩子：starts=%d", starts)
	}
	if got := src.lastStart; got.PTT != "ctrl+shift+m" {
		t.Errorf("安装用的绑定 = %+v", got)
	}
}

func TestManagerEventsDeliversFakeEvents(t *testing.T) {
	m, src := newTestManager(true, enabledSettings())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	seq := []Event{
		{Action: ActionPTT, Pressed: true},
		{Action: ActionPTT, Pressed: false},
		{Action: ActionMute, Pressed: true},
		{Action: ActionDeafen, Pressed: true},
		{Action: ActionDual, Pressed: true},
	}
	for _, ev := range seq {
		if !src.push(ev) {
			t.Fatalf("通道未满时应投递成功：%+v", ev)
		}
	}
	for _, want := range seq {
		select {
		case got := <-m.Events():
			if got != want {
				t.Errorf("收到 %+v，期望 %+v", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("等事件超时：%+v", want)
		}
	}
}

func TestManagerEventsDropWhenFull(t *testing.T) {
	m, src := newTestManager(true, enabledSettings())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	for i := 0; i < eventChanCap; i++ {
		if !src.push(Event{Action: ActionPTT, Pressed: true}) {
			t.Fatalf("第 %d 条应投递成功", i)
		}
	}
	if src.push(Event{Action: ActionPTT, Pressed: false}) {
		t.Error("通道满时应丢弃，不能阻塞")
	}
	if n := len(m.Events()); n != eventChanCap {
		t.Errorf("通道内应有 %d 条，实际 %d", eventChanCap, n)
	}
}

func TestEmitDropsWhenFull(t *testing.T) {
	ch := make(chan Event, eventChanCap)
	for i := 0; i < eventChanCap; i++ {
		emit(ch, Event{Action: ActionPTT, Pressed: true})
	}
	if len(ch) != eventChanCap {
		t.Fatalf("通道内应有 %d 条，实际 %d", eventChanCap, len(ch))
	}
	emit(ch, Event{Action: ActionMute})
	if len(ch) != eventChanCap {
		t.Errorf("满通道应丢弃，实际 %d", len(ch))
	}
}

func TestSourceInterfaceSatisfiedByBothPlatforms(t *testing.T) {
	// 编译期保证：平台实现与 stub 都满足 Manager 依赖的接口。
	var _ source = (*fakeSource)(nil)
	var _ source = newPlatformSource()
}
