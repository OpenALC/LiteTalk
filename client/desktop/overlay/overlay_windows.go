//go:build windows

// Windows 实现：纯 Go syscall 直调 user32/gdi32/kernel32（沿用 core/hotkey
// 的 Win32 范式，不引入 cgo 或 golang.org/x/sys/windows）。
//
// 窗口是 layered + color-key：取一个绘制中不会出现的品红色作透明键，GDI
// 直接画到窗口 DC。不用 per-pixel alpha 的 UpdateLayeredWindow 是因为 GDI
// 文字画进 32 位 DIB 时不会填 alpha 通道，未预乘的结果要么全透明要么黑边。
//
// 线程模型：RegisterClassExW/CreateWindowExW 与消息泵必须同线程，窗口线程
// LockOSThread 后独占运行；快照回调也在同一线程上每 100ms 调用一次，因此
// 绘制状态（last/visible/句柄）无需加锁。Stop 从其他线程 PostMessageW(WM_CLOSE)
// 唤醒消息泵，窗口销毁后 PostQuitMessage 结束循环。
package overlay

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procDestroyWindow              = user32.NewProc("DestroyWindow")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procPostThreadMessageW         = user32.NewProc("PostThreadMessageW")
	procPostQuitMessage            = user32.NewProc("PostQuitMessage")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procSetTimer                   = user32.NewProc("SetTimer")
	procKillTimer                  = user32.NewProc("KillTimer")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procSetWindowPos               = user32.NewProc("SetWindowPos")
	procBeginPaint                 = user32.NewProc("BeginPaint")
	procEndPaint                   = user32.NewProc("EndPaint")
	procGetDC                      = user32.NewProc("GetDC")
	procReleaseDC                  = user32.NewProc("ReleaseDC")
	procValidateRect               = user32.NewProc("ValidateRect")
	procFillRect                   = user32.NewProc("FillRect")
	procDrawTextW                  = user32.NewProc("DrawTextW")
	procMonitorFromWindow          = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW            = user32.NewProc("GetMonitorInfoW")
	procGetDpiForSystem            = user32.NewProc("GetDpiForSystem")

	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW           = gdi32.NewProc("CreateFontW")
	procSelectObject          = gdi32.NewProc("SelectObject")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
	procGetStockObject        = gdi32.NewProc("GetStockObject")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procEllipse               = gdi32.NewProc("Ellipse")
	procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")
	procGetDeviceCaps         = gdi32.NewProc("GetDeviceCaps")

	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
)

const (
	wsPopup = 0x80000000

	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExNoActivate  = 0x08000000

	swHide = 0

	wmDestroy       = 0x0002
	wmPaint         = 0x000F
	wmClose         = 0x0010
	wmQuit          = 0x0012
	wmEraseBkgnd    = 0x0014
	wmMouseActivate = 0x0021
	wmNCHitTest     = 0x0084
	wmTimer         = 0x0113

	maNoActivate = 3

	lwaColorKey   = 0x00000001
	bkTransparent = 1

	logPixelsX              = 88
	monitorDefaultToPrimary = 1
	nullPen                 = 8
	defaultCharset          = 1
	cleartypeQuality        = 5
	fwNormal                = 400

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	dtRight       = 0x00000002
	dtVCenter     = 0x00000004
	dtSingleLine  = 0x00000020
	dtNoPrefix    = 0x00000800
	dtEndEllipsis = 0x00008000

	timerID       = 1
	timerInterval = 100 // ms；只做快照比较，变化时才重绘

	classAlreadyExists = 1410 // ERROR_CLASS_ALREADY_EXISTS
)

// 布局尺寸以 96 DPI 为基准，运行时按系统 DPI 缩放。
const (
	panelWidthDip = 232
	rowHeightDip  = 30
	paddingDip    = 10
	marginDip     = 16
	dotRadiusDip  = 4
	gapDip        = 8
	fontPointSize = 12
)

const stopTimeout = 3 * time.Second

var (
	// 颜色全部为 COLORREF（0x00BBGGRR）。
	panelColor = rgb(0x17, 0x32, 0x4B) // 品牌蓝灰深底
	textColor  = rgb(0xFF, 0xFF, 0xFF)
	tagColor   = rgb(0xA8, 0xBE, 0xD2)
	// 透明键：品红在面板绘制中不会出现，作为 color-key 不会误伤内容。
	colorKey = rgb(0xFF, 0x00, 0xFF)
)

type rect struct{ left, top, right, bottom int32 }
type point struct{ x, y int32 }
type size struct{ cx, cy int32 }

// wndClassEx 对应 Win32 WNDCLASSEXW（64 位布局 80 字节）。
type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// msg 对应 Win32 MSG。message 后的 4 字节由 Go 自动补齐，与原生布局一致。
type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// paintStruct 对应 Win32 PAINTSTRUCT（64 位 72 字节）。
type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

// monitorInfo 对应 Win32 MONITORINFO。
type monitorInfo struct {
	cbSize    uint32
	rcMonitor rect
	rcWork    rect
	dwFlags   uint32
}

var (
	// wndProcOnce 保证 NewCallback 只调用一次：反复开关悬浮层不应持续消耗
	// Go 的回调槽位（每个进程数量有限）。
	wndProcOnce sync.Once
	wndProcAddr uintptr

	// wndState 仅窗口线程读写（含 wndProc 回调），无需加锁。
	wndState *windowState

	winMu sync.Mutex
	win   *winWindow
)

func supportedPlatform() bool {
	for _, p := range []*syscall.LazyProc{
		procRegisterClassExW, procCreateWindowExW, procGetMessageW,
		procSetLayeredWindowAttributes, procSetTimer, procBeginPaint,
		procEndPaint, procFillRect, procDrawTextW, procCreateFontW,
		procSelectObject, procCreateSolidBrush, procMonitorFromWindow,
		procGetMonitorInfoW, procShowWindow, procSetWindowPos,
	} {
		if p.Find() != nil {
			return false
		}
	}
	return true
}

// winWindow 表示一次 Start/Stop 周期的窗口线程。
type winWindow struct {
	snapshot Snapshot
	ready    chan error
	done     chan struct{}

	// hwnd/tid 在窗口线程上写入、在 ready 发送之前完成；Start 收到 nil 后
	// 才会把 w 发布给 Stop，channel 建立了 happens-before。
	hwnd uintptr
	tid  uint32
}

func startPlatform(snapshot Snapshot) error {
	w := &winWindow{
		snapshot: snapshot,
		ready:    make(chan error, 1),
		done:     make(chan struct{}),
	}
	go w.run()
	if err := <-w.ready; err != nil {
		<-w.done
		return err
	}
	winMu.Lock()
	win = w
	winMu.Unlock()
	return nil
}

func stopPlatform() {
	winMu.Lock()
	w := win
	win = nil
	winMu.Unlock()
	if w != nil {
		w.stop()
	}
}

// stop 请求窗口线程关闭并等待其退出。超时只记录日志：进程退出时窗口句柄
// 随进程回收，不能因为异常消息泵把整个关闭流程卡死。
func (w *winWindow) stop() {
	if w.hwnd != 0 {
		if r, _, _ := procPostMessageW.Call(w.hwnd, wmClose, 0, 0); r == 0 && w.tid != 0 {
			procPostThreadMessageW.Call(uintptr(w.tid), wmQuit, 0, 0)
		}
	} else if w.tid != 0 {
		// 窗口尚未建好（正常情况下 Start 返回前已建好）：直接让消息泵退出。
		procPostThreadMessageW.Call(uintptr(w.tid), wmQuit, 0, 0)
	}
	select {
	case <-w.done:
	case <-time.After(stopTimeout):
		log.Printf("overlay: 窗口线程 %s 内未退出", stopTimeout)
	}
}

func (w *winWindow) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(w.done)

	if tid, _, _ := procGetCurrentThreadId.Call(); tid != 0 {
		w.tid = uint32(tid)
	}

	st, hwnd, err := createWindow(w.snapshot)
	if err != nil {
		w.ready <- err
		return
	}
	w.hwnd = hwnd
	wndState = st
	w.ready <- nil

	var m msg
	for {
		// 0 = WM_QUIT；-1 = 取消息失败。两者都结束循环。
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	wndState = nil
	st.destroyGDI()
}

// windowState 是窗口线程独占的绘制状态。
type windowState struct {
	snapshot Snapshot
	hwnd     uintptr
	dpi      uint32

	x, y          int32
	width, height int32

	font       uintptr
	panelBrush uintptr
	dotBrushes [4]uintptr
	nullPen    uintptr

	last    []Speaker
	visible bool
}

func createWindow(snapshot Snapshot) (*windowState, uintptr, error) {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, err := syscall.UTF16PtrFromString("LiteTalkOverlayWindow")
	if err != nil {
		return nil, 0, fmt.Errorf("overlay: 窗口类名: %w", err)
	}
	title, err := syscall.UTF16PtrFromString("轻音 LiteTalk")
	if err != nil {
		return nil, 0, fmt.Errorf("overlay: 窗口标题: %w", err)
	}

	wndProcOnce.Do(func() { wndProcAddr = syscall.NewCallback(overlayWndProc) })
	wc := wndClassEx{
		lpfnWndProc:   wndProcAddr,
		hInstance:     hInst,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 &&
		!isErrno(callErr, classAlreadyExists) {
		return nil, 0, fmt.Errorf("overlay: RegisterClassExW: %v", callErr)
	}

	exStyle := uintptr(wsExLayered | wsExTransparent | wsExTopmost | wsExToolWindow | wsExNoActivate)
	hwnd, _, callErr := procCreateWindowExW.Call(
		exStyle,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup),
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		return nil, 0, fmt.Errorf("overlay: CreateWindowExW: %v", callErr)
	}

	if r, _, callErr := procSetLayeredWindowAttributes.Call(
		hwnd, uintptr(colorKey), 0, lwaColorKey); r == 0 {
		procDestroyWindow.Call(hwnd)
		return nil, 0, fmt.Errorf("overlay: SetLayeredWindowAttributes: %v", callErr)
	}

	st := newWindowState(snapshot, hwnd)
	if st.font == 0 || st.panelBrush == 0 || st.nullPen == 0 ||
		st.dotBrushes[0] == 0 || st.dotBrushes[1] == 0 ||
		st.dotBrushes[2] == 0 || st.dotBrushes[3] == 0 {
		st.destroyGDI()
		procDestroyWindow.Call(hwnd)
		return nil, 0, errors.New("overlay: 创建 GDI 资源失败")
	}

	// 主显示器工作区左上角（MVP 不做多显示器/拖拽配置）。
	mon, _, _ := procMonitorFromWindow.Call(0, monitorDefaultToPrimary)
	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r != 0 {
		st.x = mi.rcWork.left + st.dp(marginDip)
		st.y = mi.rcWork.top + st.dp(marginDip)
	} else {
		st.x, st.y = st.dp(marginDip), st.dp(marginDip)
	}

	if r, _, callErr := procSetTimer.Call(hwnd, timerID, timerInterval, 0); r == 0 {
		st.destroyGDI()
		procDestroyWindow.Call(hwnd)
		return nil, 0, fmt.Errorf("overlay: SetTimer: %v", callErr)
	}
	return st, hwnd, nil
}

func newWindowState(snapshot Snapshot, hwnd uintptr) *windowState {
	st := &windowState{snapshot: snapshot, hwnd: hwnd, dpi: systemDPI()}
	if b, _, _ := procCreateSolidBrush.Call(uintptr(panelColor)); b != 0 {
		st.panelBrush = b
	}
	for i, c := range identityColors {
		if b, _, _ := procCreateSolidBrush.Call(uintptr(c)); b != 0 {
			st.dotBrushes[i] = b
		}
	}
	if p, _, _ := procGetStockObject.Call(nullPen); p != 0 {
		st.nullPen = p
	}
	st.font = st.createFont()
	return st
}

func (st *windowState) createFont() uintptr {
	face, err := syscall.UTF16PtrFromString("Microsoft YaHei UI")
	if err != nil {
		return 0
	}
	// 负高度 = 字符高度；缺字体时 GDI 字体映射器自动回落到 Segoe UI。
	h := int32(-st.pt(fontPointSize))
	f, _, _ := procCreateFontW.Call(
		uintptr(h), 0, 0, 0, fwNormal, 0, 0, 0,
		defaultCharset, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(face)),
	)
	runtime.KeepAlive(face)
	return f
}

// dp 把 96 DPI 基准尺寸缩放成物理像素。
func (st *windowState) dp(v int32) int32 {
	return int32(int64(v) * int64(st.dpi) / 96)
}

// pt 把磅值换算成物理像素（CreateFontW 的字符高度）。
func (st *windowState) pt(v int32) int32 {
	return int32(int64(v) * int64(st.dpi) / 72)
}

func systemDPI() uint32 {
	if procGetDpiForSystem.Find() == nil {
		if r, _, _ := procGetDpiForSystem.Call(); r >= 96 && r <= 480 {
			return uint32(r)
		}
	}
	// 旧系统没有 GetDpiForSystem：用屏幕 DC 的 LOGPIXELSX 回落。
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 96
	}
	r, _, _ := procGetDeviceCaps.Call(hdc, logPixelsX)
	procReleaseDC.Call(0, hdc)
	if r >= 96 && r <= 480 {
		return uint32(r)
	}
	return 96
}

// poll 是 100ms 定时器主体：取快照、比较、按需重绘/显示/隐藏。
func (st *windowState) poll(hwnd uintptr) {
	speakers := compose(safeSnapshot(st.snapshot))
	if sameSpeakers(speakers, st.last) {
		return
	}
	st.last = speakers
	if len(speakers) == 0 {
		if st.visible {
			procShowWindow.Call(hwnd, swHide)
			st.visible = false
		}
		return
	}
	st.resize(hwnd)
	st.redraw(hwnd)
	if !st.visible {
		// 重画完再显示，避免先露出未绘制的一帧；顶层窗口重新断言 topmost。
		procSetWindowPos.Call(hwnd, ^uintptr(0), 0, 0, 0, 0,
			swpNoMove|swpNoSize|swpNoActivate|swpShowWindow)
		st.visible = true
	}
}

func (st *windowState) resize(hwnd uintptr) {
	st.width = st.dp(panelWidthDip)
	st.height = st.dp(paddingDip)*2 + int32(len(st.last))*st.dp(rowHeightDip)
	procSetWindowPos.Call(hwnd, 0,
		uintptr(st.x), uintptr(st.y), uintptr(st.width), uintptr(st.height),
		swpNoZOrder|swpNoActivate)
}

// redraw 直接画到窗口 DC 并清掉更新区：隐藏窗口也能先画好再显示，
// 避免 WM_PAINT 的时序造成闪烁。
func (st *windowState) redraw(hwnd uintptr) {
	if hdc, _, _ := procGetDC.Call(hwnd); hdc != 0 {
		st.draw(hdc)
		procReleaseDC.Call(hwnd, hdc)
	}
	procValidateRect.Call(hwnd, 0)
}

func (st *windowState) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc != 0 {
		st.draw(hdc)
	}
	procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

func (st *windowState) draw(hdc uintptr) {
	if len(st.last) == 0 {
		return
	}
	bg := rect{0, 0, st.width, st.height}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&bg)), st.panelBrush)
	procSelectObject.Call(hdc, st.font)
	procSetBkMode.Call(hdc, bkTransparent)
	oldPen, _, _ := procSelectObject.Call(hdc, st.nullPen)
	defer procSelectObject.Call(hdc, oldPen)

	rowH := st.dp(rowHeightDip)
	pad := st.dp(paddingDip)
	radius := st.dp(dotRadiusDip)
	gap := st.dp(gapDip)
	rowRight := st.width - pad

	for i, sp := range st.last {
		top := pad + int32(i)*rowH
		cy := top + rowH/2
		cx := pad + radius
		brush := st.dotBrushes[IdentityColorIndex(sp.UID)]
		oldBrush, _, _ := procSelectObject.Call(hdc, brush)
		procEllipse.Call(hdc,
			uintptr(cx-radius), uintptr(cy-radius),
			uintptr(cx+radius), uintptr(cy+radius))
		procSelectObject.Call(hdc, oldBrush)

		textLeft := cx + radius + st.dp(6)
		textRight := rowRight
		if tag := statusText(sp); tag != "" {
			if u16, err := syscall.UTF16FromString(tag); err == nil {
				var sz size
				procGetTextExtentPoint32W.Call(hdc,
					uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1),
					uintptr(unsafe.Pointer(&sz)))
				tagRect := rect{rowRight - sz.cx, top, rowRight, top + rowH}
				procSetTextColor.Call(hdc, uintptr(tagColor))
				procDrawTextW.Call(hdc,
					uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1),
					uintptr(unsafe.Pointer(&tagRect)),
					dtSingleLine|dtVCenter|dtRight|dtNoPrefix)
				textRight = rowRight - sz.cx - gap
				runtime.KeepAlive(u16)
			}
		}
		if textRight <= textLeft {
			continue
		}
		if u16, err := syscall.UTF16FromString(sp.Nickname); err == nil {
			nameRect := rect{textLeft, top, textRight, top + rowH}
			procSetTextColor.Call(hdc, uintptr(textColor))
			procDrawTextW.Call(hdc,
				uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1),
				uintptr(unsafe.Pointer(&nameRect)),
				dtSingleLine|dtVCenter|dtEndEllipsis|dtNoPrefix)
			runtime.KeepAlive(u16)
		}
	}
}

func (st *windowState) destroyGDI() {
	for _, h := range []*uintptr{&st.font, &st.panelBrush} {
		if *h != 0 {
			procDeleteObject.Call(*h)
			*h = 0
		}
	}
	for i := range st.dotBrushes {
		if st.dotBrushes[i] != 0 {
			procDeleteObject.Call(st.dotBrushes[i])
			st.dotBrushes[i] = 0
		}
	}
	// nullPen 是 stock object，不删除。
}

func statusText(sp Speaker) string {
	var parts [3]string
	n := 0
	if sp.Mute {
		parts[n] = "闭麦"
		n++
	}
	if sp.Deafen {
		parts[n] = "静音"
		n++
	}
	if sp.Priority {
		parts[n] = "优先"
		n++
	}
	switch n {
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " · " + parts[1]
	case 3:
		return parts[0] + " · " + parts[1] + " · " + parts[2]
	}
	return ""
}

// overlayWndProc 是窗口线程上的消息处理。禁止激活/命中测试让窗口完全不
// 参与焦点与鼠标交互（点击穿透）。
func overlayWndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	st := wndState
	switch uint32(message) {
	case wmMouseActivate:
		return maNoActivate
	case wmNCHitTest:
		return ^uintptr(0) // HTTRANSPARENT
	case wmEraseBkgnd:
		return 1 // 全部由 WM_PAINT/直接绘制覆盖，避免闪烁
	case wmPaint:
		if st != nil {
			st.paint(hwnd)
		} else {
			var ps paintStruct
			procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
			procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		}
		return 0
	case wmTimer:
		if st != nil && wParam == timerID {
			st.poll(hwnd)
		}
		return 0
	case wmClose:
		if st != nil {
			procKillTimer.Call(hwnd, timerID)
		}
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

func isErrno(err error, code syscall.Errno) bool {
	e, ok := err.(syscall.Errno)
	return ok && e == code
}
