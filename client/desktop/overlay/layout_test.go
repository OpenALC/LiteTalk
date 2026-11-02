package overlay

import "testing"

// TestValidDPI DPI 判定表：合法区间原样返回，越界一律回落 96（#117）。
func TestValidDPI(t *testing.T) {
	cases := []struct {
		in, want uint32
	}{
		{96, 96},   // 100%
		{120, 120}, // 125%
		{144, 144}, // 150%
		{192, 192}, // 200%
		{480, 480}, // 500% 上限
		{0, 96},    // API 失败
		{1, 96},
		{95, 96},
		{481, 96},
		{0xFFFF, 96},
	}
	for _, c := range cases {
		if got := validDPI(c.in); got != c.want {
			t.Errorf("validDPI(%d) = %d，want %d", c.in, got, c.want)
		}
	}
}

// TestScaleDPI 缩放换算：整数运算，非整数倍下按 96 基准等比放大。
func TestScaleDPI(t *testing.T) {
	cases := []struct {
		dip, dpi uint32
		want     int32
	}{
		{232, 96, 232},
		{232, 120, 290}, // 232*1.25
		{232, 144, 348}, // 232*1.5
		{232, 192, 464}, // 232*2
		{30, 120, 37},   // 37.5 截断
		{16, 144, 24},
		{232, 0, 232},    // 非法 DPI 按 96 处理
		{232, 480, 1160}, // 500%
	}
	for _, c := range cases {
		if got := scaleDPI(c.dip, c.dpi); got != c.want {
			t.Errorf("scaleDPI(%d, %d) = %d，want %d", c.dip, c.dpi, got, c.want)
		}
	}
}

// TestFontHeight 磅值换算：12pt @96DPI = 16px，@144DPI = 24px。
func TestFontHeight(t *testing.T) {
	cases := []struct {
		pt, dpi uint32
		want    int32
	}{
		{12, 96, 16},
		{12, 120, 20},
		{12, 144, 24},
		{12, 0, 16}, // 非法 DPI 回落
	}
	for _, c := range cases {
		if got := fontHeight(c.pt, c.dpi); got != c.want {
			t.Errorf("fontHeight(%d, %d) = %d，want %d", c.pt, c.dpi, got, c.want)
		}
	}
}

// TestPanelSize 面板尺寸随说话人数线性增长，缩放一并生效。
func TestPanelSize(t *testing.T) {
	cases := []struct {
		dpi   uint32
		rows  int
		wantW int32
		wantH int32
	}{
		{96, 1, 232, 50},   // 10*2 + 1*30
		{96, 4, 232, 140},  // 10*2 + 4*30
		{144, 1, 348, 75},  // 15*2 + 1*45
		{144, 4, 348, 210}, // 15*2 + 4*45
		{120, 4, 290, 172}, // 12*2 + 4*37
	}
	for _, c := range cases {
		w, h := panelSize(c.dpi, c.rows)
		if w != c.wantW || h != c.wantH {
			t.Errorf("panelSize(%d, %d) = (%d, %d)，want (%d, %d)",
				c.dpi, c.rows, w, h, c.wantW, c.wantH)
		}
	}
	// 无说话者时高度只剩内边距：显示前调用方会先隐藏，这里只确认不出现负值。
	if w, h := panelSize(96, 0); w != 232 || h != 20 {
		t.Errorf("panelSize(96, 0) = (%d, %d)，want (232, 20)", w, h)
	}
}

// TestAnchorInWorkArea 定位 = 工作区（已扣任务栏）左上角 + 边距；副屏负坐标原样保留。
func TestAnchorInWorkArea(t *testing.T) {
	cases := []struct {
		left, top int32
		dpi       uint32
		wantX     int32
		wantY     int32
	}{
		{0, 0, 96, 16, 16},        // 主屏
		{1920, 0, 96, 1936, 16},   // 右侧副屏
		{-1920, 0, 96, -1904, 16}, // 主屏左侧副屏：负坐标必须原样保留
		{0, -120, 144, 24, -96},   // 上方副屏 + 150% 缩放
		{0, 40, 120, 20, 60},      // 顶部任务栏：工作区 top=40
	}
	for _, c := range cases {
		x, y := anchorInWorkArea(c.left, c.top, c.dpi)
		if x != c.wantX || y != c.wantY {
			t.Errorf("anchorInWorkArea(%d, %d, %d) = (%d, %d)，want (%d, %d)",
				c.left, c.top, c.dpi, x, y, c.wantX, c.wantY)
		}
	}
}
