package overlay

// 悬浮层的尺寸与定位计算与 Win32 调用分离，放在平台无关文件里：
// CI 的 Linux 检查 job 不编译 overlay_windows.go，只有这里能被两边跑到，
// 判定表也就能在非 Windows 上守着（#117）。Windows 实现只负责把这里的
// 数值喂给 SetWindowPos / CreateFontW。

// 布局尺寸以 96 DPI 为基准，运行时按显示器 DPI 缩放。
const (
	panelWidthDip = 232
	rowHeightDip  = 30
	paddingDip    = 10
	marginDip     = 16
	dotRadiusDip  = 4
	gapDip        = 8

	// minDPI/maxDPI 是 DPI 的合法区间：低于 96 只可能是 API 异常（Windows
	// 最小缩放 100%），高于 480（500%）则明显是取错了值。
	minDPI = 96
	maxDPI = 480
)

// validDPI 把外部传入的 DPI 约束到合法区间，异常值回落 96（100%）。
func validDPI(dpi uint32) uint32 {
	if dpi < minDPI || dpi > maxDPI {
		return minDPI
	}
	return dpi
}

// scaleDPI 把 96 DPI 基准的尺寸换算成目标 DPI 下的物理像素。
// 先乘后除、整数运算，避免 125%/150% 这类非整数倍下的累积偏差。
func scaleDPI(v, dpi uint32) int32 {
	return int32(int64(v) * int64(validDPI(dpi)) / 96)
}

// fontHeight 磅值到物理像素（CreateFontW 的字符高度取负值=字高）。
func fontHeight(pointSize, dpi uint32) int32 {
	return int32(int64(pointSize) * int64(validDPI(dpi)) / 72)
}

// panelSize 悬浮层面板尺寸：宽度固定，高度随说话人数增长。
func panelSize(dpi uint32, rows int) (width, height int32) {
	width = scaleDPI(panelWidthDip, dpi)
	height = scaleDPI(paddingDip, dpi)*2 + int32(rows)*scaleDPI(rowHeightDip, dpi)
	return width, height
}

// anchorInWorkArea 把面板定位到工作区（已扣除任务栏的区域）左上角并留边距。
// 工作区可能是负坐标（主屏左侧/上方的副屏），不能假设从 0 开始。
func anchorInWorkArea(workLeft, workTop int32, dpi uint32) (x, y int32) {
	m := scaleDPI(marginDip, dpi)
	return workLeft + m, workTop + m
}
