package audio

import "testing"

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
