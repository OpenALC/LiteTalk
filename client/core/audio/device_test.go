package audio

import (
	"testing"

	"github.com/gen2brain/malgo"
)

// TestNewContext 真正调用 malgo.InitContext，挡住 cgo 指针传参回归。
// 原 bug 把 Go 指针塞进 C 结构体（PApplicationName = &slice[0]），cgocheck 在
// InitContext 调用边界直接 panic 崩进程；panic 与有无音频设备无关，所以无声卡的
// CI 环境同样能挡住回归。后端不可用时记日志早退而不是 t.Skip。
func TestNewContext(t *testing.T) {
	ctx, err := NewContext(nil)
	if err != nil {
		// 后端不可用（无声卡/无音频服务）。cgo 指针检查已在调用边界完成，
		// 走到这里就说明传参合法；设备缺失不构成本测试的失败。
		t.Logf("audio backend unavailable: %v", err)
		return
	}
	defer ctx.Close()

	// 初始化成功时枚举必须可用：无设备返回空列表，而不是错误。
	if _, err := ctx.Devices(malgo.Capture); err != nil {
		t.Errorf("enumerate capture devices: %v", err)
	}
	if _, err := ctx.Devices(malgo.Playback); err != nil {
		t.Errorf("enumerate playback devices: %v", err)
	}
}

// TestPickDevice 设备 selector 解析：空 = 默认、精确 ID、名称子串（忽略大小写）、无匹配。
func TestPickDevice(t *testing.T) {
	devs := []DeviceInfo{
		{ID: "a1b2", Name: "Microphone Array (Realtek)"},
		{ID: "c3d4", Name: "Speakers (Realtek)"},
		{ID: "e5f6", Name: "Headphones"},
	}

	if got := pickDevice(devs, ""); got != -1 {
		t.Errorf("empty selector = %d, want -1 (default)", got)
	}
	if got := pickDevice(devs, "c3d4"); got != 1 {
		t.Errorf("exact ID = %d, want 1", got)
	}
	if got := pickDevice(devs, "headphones"); got != 2 {
		t.Errorf("case-insensitive name substring = %d, want 2", got)
	}
	if got := pickDevice(devs, "REALTEK"); got != 0 {
		// 两个都含 realtek，取第一个
		t.Errorf("name substring first hit = %d, want 0", got)
	}
	if got := pickDevice(devs, "不存在的设备"); got != -2 {
		t.Errorf("no match = %d, want -2", got)
	}
}

// TestPickDeviceExactIDBeatsName 精确 ID 优先于名称子串。
func TestPickDeviceExactIDBeatsName(t *testing.T) {
	devs := []DeviceInfo{
		{ID: "mic", Name: "Speakers"},
		{ID: "spk", Name: "Mic"},
	}
	if got := pickDevice(devs, "mic"); got != 0 {
		t.Errorf("exact ID match = %d, want 0 (ID 优先)", got)
	}
}
