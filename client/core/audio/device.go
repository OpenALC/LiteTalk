package audio

/*
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// DeviceInfo 是供上层使用的设备描述（ID 为 malgo 原始 ID 的十六进制形式）。
type DeviceInfo struct {
	ID        string
	Name      string
	IsDefault bool
}

// Context 包装 malgo 上下文；进程内建议只创建一个。
// pulseName 是交给 Pulse 后端的应用名（C 内存），随上下文一起释放。
type Context struct {
	ctx       *malgo.AllocatedContext
	pulseName *C.char
}

// NewContext 初始化音频后端上下文（Windows 上为 WASAPI）。
// logf 为 nil 时静默；否则接收 miniaudio 日志。
func NewContext(logf func(string)) (*Context, error) {
	if logf == nil {
		logf = func(string) {}
	}
	// PulseAudio 后端在 connect 时读 pApplicationName（pavucontrol 等音量面板按它
	// 显示应用名）。malgo 的 DeviceConfig.Pulse 只暴露 StreamName*，应用名在
	// ContextConfig.Pulse.PApplicationName——只能在 init context 时设置一次。
	// 该字段只被 Pulse 后端读取，其它后端忽略。
	//
	// 必须用 C 内存：cfg 是 Go 侧的 C 结构体值，字段是 *byte；若指向 Go 内存
	// （例如 &slice[0]），就构成"传给 C 的 Go 指针所指内存里含 Go 指针"，
	// cgocheck 在调用边界直接 panic——与 C 是否留存、指针是否 KeepAlive 无关。
	// C.CString 分配在 C 堆、GC 不碰，可安全让 C 留存。miniaudio 在 ma_context_init
	// 里 ma_copy_string 复制一份，本份由 Context 持有、Close 时释放。
	pulseName := C.CString("LiteTalk")
	cfg := malgo.ContextConfig{}
	cfg.Pulse.PApplicationName = (*byte)(unsafe.Pointer(pulseName))
	ctx, err := malgo.InitContext(nil, cfg, logf)
	if err != nil {
		C.free(unsafe.Pointer(pulseName))
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	return &Context{ctx: ctx, pulseName: pulseName}, nil
}

// Close 释放上下文。调用前必须先停掉所有由它创建的设备。
func (c *Context) Close() error {
	if c.ctx == nil {
		return nil
	}
	err := c.ctx.Uninit()
	c.ctx.Free()
	c.ctx = nil
	if c.pulseName != nil {
		C.free(unsafe.Pointer(c.pulseName))
		c.pulseName = nil
	}
	return err
}

// Devices 枚举采集（malgo.Capture）或播放（malgo.Playback）设备。
func (c *Context) Devices(kind malgo.DeviceType) ([]DeviceInfo, error) {
	infos, err := c.ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("enumerate devices: %w", err)
	}
	out := make([]DeviceInfo, len(infos))
	for i := range infos {
		out[i] = DeviceInfo{
			ID:        infos[i].ID.String(),
			Name:      infos[i].Name(),
			IsDefault: infos[i].IsDefault != 0,
		}
	}
	return out, nil
}

// pickDevice 按 selector 在设备列表中选设备（纯函数，便于单测）：
// selector 为空 → -1（默认设备）；先精确匹配 ID，再大小写不敏感子串匹配名称；
// 无匹配 → -2。
func pickDevice(devices []DeviceInfo, selector string) int {
	if selector == "" {
		return -1
	}
	for i := range devices {
		if devices[i].ID == selector {
			return i
		}
	}
	lower := strings.ToLower(selector)
	for i := range devices {
		if strings.Contains(strings.ToLower(devices[i].Name), lower) {
			return i
		}
	}
	return -2
}

// nativeCaptureChannels 查询采集设备的原生声道数（上限 4）。
// 查询失败/信息缺失回落 1（等价 miniaudio 矩形下混，不劣于修复前）。
// devID 为 nil 表示系统默认设备；非 nil 时优先按 ID 精确查询。
func (c *Context) nativeCaptureChannels(devID *malgo.DeviceID) int {
	const maxChannels = 4
	chFrom := func(info malgo.DeviceInfo) int {
		for _, f := range info.Formats {
			if f.Channels > 0 {
				n := int(f.Channels)
				if n > maxChannels {
					n = maxChannels
				}
				return n
			}
		}
		return 1
	}
	if devID != nil {
		info, err := c.ctx.DeviceInfo(malgo.Capture, *devID, malgo.Shared)
		if err == nil {
			return chFrom(info)
		}
	}
	// 默认设备或精确查询失败：回落枚举列表（IsDefault / ID 匹配）。
	infos, err := c.ctx.Devices(malgo.Capture)
	if err != nil || len(infos) == 0 {
		return 1
	}
	idx := -1
	if devID == nil {
		for i := range infos {
			if infos[i].IsDefault != 0 {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = 0
		}
	} else {
		for i := range infos {
			if infos[i].ID == *devID {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return 1
	}
	return chFrom(infos[idx])
}

// resolveDeviceID 把用户的选择器解析为 malgo 设备 ID 指针。
// selector 为空 → nil（默认设备）；否则先精确匹配 ID 十六进制串，
// 再大小写不敏感子串匹配设备名；都不中则报错并列出候选。
func (c *Context) resolveDeviceID(kind malgo.DeviceType, selector string) (*malgo.DeviceID, error) {
	if selector == "" {
		return nil, nil
	}
	infos, err := c.ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("enumerate devices: %w", err)
	}
	kindName := "capture"
	if kind == malgo.Playback {
		kindName = "playback"
	}
	list := make([]DeviceInfo, len(infos))
	for i := range infos {
		list[i] = DeviceInfo{ID: infos[i].ID.String(), Name: infos[i].Name()}
	}
	if idx := pickDevice(list, selector); idx >= 0 {
		return &infos[idx].ID, nil
	}
	names := make([]string, len(infos))
	for i := range infos {
		names[i] = infos[i].Name()
	}
	return nil, fmt.Errorf("no %s device matches %q (available: %s)", kindName, selector, strings.Join(names, "; "))
}
