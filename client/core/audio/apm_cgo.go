//go:build cgo && (windows || (linux && (amd64 || arm64)))

// WebRTC APM 的 cgo 封装。静态库与头文件都在仓库内 third_party/webrtc-apm/，
// 经 ${SRCDIR} 相对路径引用，任何机器 go build 免 pkg-config、免额外环境变量。
//
// 平台矩阵：Windows amd64、Linux amd64/arm64 各有一套预编译静态库；
// 其余平台由 apm_stub.go 提供 no-op 实现（apmSupported 为 false）。
// 库名统一为 libwebrtc-apm.a（已合并 abseil/webrtc 全部依赖对象）。
package audio

/*
// CXXFLAGS 用 gnu++17 而非 c++17：GCC 16 的 libstdc++ 在严格 ISO 模式下需要
// -fext-numeric-literals 才能解析 __float128 的 Q 后缀，而 cgo 拒绝该参数。
#cgo CXXFLAGS: -std=gnu++17 -DNDEBUG -DWEBRTC_APM_DEBUG_DUMP=0 -D__STDC_FORMAT_MACROS=1 -I${SRCDIR}/../../third_party/webrtc-apm/include/webrtc-audio-processing-1
#cgo windows CXXFLAGS: -DWEBRTC_WIN -D_WIN32 -DNOMINMAX -D_USE_MATH_DEFINES
#cgo linux CXXFLAGS: -DWEBRTC_LINUX -DWEBRTC_POSIX -DWEBRTC_THREAD_RR
// Windows 必须纯静态（无第三方 DLL）：显式把 libstdc++/winpthread 切到静态归档，
// 只保留 KERNEL32/WINMM/UCRT 系统 DLL 依赖。--start-group 兜住合并归档内部
// 的跨成员引用（abseil/webrtc 互相引用）。
#cgo windows LDFLAGS: -L${SRCDIR}/../../third_party/webrtc-apm/lib/windows-amd64 -Wl,--start-group -lwebrtc-apm -Wl,--end-group -lwinmm -Wl,-Bstatic -lstdc++ -lwinpthread -Wl,-Bdynamic -static-libgcc -static-libstdc++
#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/../../third_party/webrtc-apm/lib/linux-amd64 -Wl,--start-group -lwebrtc-apm -Wl,--end-group -lm -lpthread -lstdc++
#cgo linux,arm64 LDFLAGS: -L${SRCDIR}/../../third_party/webrtc-apm/lib/linux-arm64 -Wl,--start-group -lwebrtc-apm -Wl,--end-group -lm -lpthread -lstdc++

#include <stdint.h>

struct lt_apm;
struct lt_apm *lt_apm_create(int aec, int ns, int ns_level, int agc2, int hpf);
void lt_apm_destroy(struct lt_apm *h);
int lt_apm_configure(struct lt_apm *h, int aec, int ns, int ns_level, int agc2, int hpf);
int lt_apm_process_capture(struct lt_apm *h, int16_t *frame, int samples);
int lt_apm_process_render(struct lt_apm *h, int16_t *frame, int samples);
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// apmFrameSize APM 只接受 10ms @ 48kHz 单声道帧，与管线帧结构同值。
const apmFrameSize = FrameSize

// apmSupported 报告当前平台是否带真实 APM 实现。
func apmSupported() bool { return true }

// apmHandle 持有 C++ APM 句柄。Close 之后不得再调用任何方法。
type apmHandle struct {
	h *C.struct_lt_apm
}

func cBoolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// newAPM 创建并初始化 APM（ApplyConfig + Initialize）。失败返回错误，
// 调用方应回落到旧 DSP 链路而不是中断音频。
func newAPM(opts apmOptions) (*apmHandle, error) {
	h := C.lt_apm_create(cBoolInt(opts.AEC), cBoolInt(opts.NS), C.int(opts.NSLevel),
		cBoolInt(opts.AGC2), cBoolInt(opts.HPF))
	if h == nil {
		return nil, fmt.Errorf("webrtc apm: create/initialize failed")
	}
	return &apmHandle{h: h}, nil
}

// configure 运行期重配置子模块（用户切换 AEC/AGC）。内部与 Process 共用
// 同一把 C++ 互斥锁，可跨 goroutine 调用。
func (a *apmHandle) configure(opts apmOptions) error {
	if a == nil || a.h == nil {
		return errAPMUnsupported
	}
	if code := C.lt_apm_configure(a.h, cBoolInt(opts.AEC), cBoolInt(opts.NS),
		C.int(opts.NSLevel), cBoolInt(opts.AGC2), cBoolInt(opts.HPF)); code != 0 {
		return fmt.Errorf("webrtc apm: configure failed (%d)", int(code))
	}
	return nil
}

// processCapture 就地处理一帧近端采集（480 采样）。pcm 长度必须 >= 480。
func (a *apmHandle) processCapture(pcm []int16) error {
	if len(pcm) < apmFrameSize {
		return fmt.Errorf("webrtc apm: capture frame %d samples, want %d", len(pcm), apmFrameSize)
	}
	if code := C.lt_apm_process_capture(a.h,
		(*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(apmFrameSize)); code != 0 {
		return fmt.Errorf("webrtc apm: process capture failed (%d)", int(code))
	}
	return nil
}

// processRender 处理一帧远端渲染参考（480 采样），必须在对应近端帧之前喂入。
// 允许 src==dst 的就地处理；本封装直接复用调用方缓冲。
func (a *apmHandle) processRender(pcm []int16) error {
	if len(pcm) < apmFrameSize {
		return fmt.Errorf("webrtc apm: render frame %d samples, want %d", len(pcm), apmFrameSize)
	}
	if code := C.lt_apm_process_render(a.h,
		(*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(apmFrameSize)); code != 0 {
		return fmt.Errorf("webrtc apm: process render failed (%d)", int(code))
	}
	return nil
}

// close 释放句柄。幂等；调用后不得再 Process。
//
// 这里直接写 a.h 而不持任何锁——安全性完全依赖 DSP 的用法约定：调用方只在
// apmMu 写锁下摘除句柄，且 Process/ProcessRender 持读锁贯穿整帧的 C 调用，
// 所以拿到写锁时必定没有读者，此后也不会再有新读者拿到这个已摘下的句柄。
// 不要为了「更安全」把读锁改短或让 Process 里缓存句柄指针跨帧使用——那会让
// a.h 变成与 lt_apm_destroy 并发访问的悬垂指针（C 堆 UAF）。
func (a *apmHandle) close() {
	if a == nil || a.h == nil {
		return
	}
	C.lt_apm_destroy(a.h)
	a.h = nil
}
