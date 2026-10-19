//go:build linux

// Package codec 是 libopus 的自研 cgo 薄封装。
//
// Linux 侧链接发行版系统库（libopus-dev 提供 opus.pc 与共享库），
// 与 Windows 侧仓库内预编译静态库不同；产物动态依赖 libopus.so。
package codec

/*
#cgo linux pkg-config: opus
#cgo linux LDFLAGS: -lopus

#include <opus.h>

// opus_encoder_ctl 在 libopus 1.5 之前是宏、之后是变参函数，cgo 对接
// 两种形态都不稳定；包一层定参静态函数统一吞掉差异。全部 SET 类 CTL
// 都只带一个整型值，一个包装即可覆盖。
static int lt_enc_ctl(OpusEncoder *e, int request, opus_int32 v) {
	return opus_encoder_ctl(e, request, v);
}
*/
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

// Opus 应用模式（值与 opus_defines.h 一致）。
const (
	AppVoIP     = 2048 // OPUS_APPLICATION_VOIP：语音优化
	AppAudio    = 2049 // OPUS_APPLICATION_AUDIO
	AppLowDelay = 2051 // OPUS_APPLICATION_RESTRICTED_LOWDELAY
)

// ErrUnsupportedPlatform 保留同名变量以维持跨平台 API 一致（Linux/Windows
// 实现均不支持平台时才返回；调用方可用 errors.Is 判断）。
var ErrUnsupportedPlatform = errors.New("opus codec: unsupported platform")

// Supported 报告当前平台是否有可用的 libopus 实现。
func Supported() bool { return true }

// Error 是 libopus 返回的负值错误码。
type Error int

func (e Error) Error() string {
	return "opus: " + C.GoString(C.opus_strerror(C.int(e)))
}

// ctlError 把 opus_encoder_ctl 的返回值转成 error（OPUS_OK=0 为成功）。
func ctlError(code C.int) error {
	if code != 0 {
		return Error(code)
	}
	return nil
}

// Encoder 封装 OpusEncoder。Encode 零分配；Close 后必须弃用。
type Encoder struct {
	st *C.OpusEncoder
}

// NewEncoder 创建编码器。application 取 AppVoIP / AppAudio / AppLowDelay。
func NewEncoder(sampleRate, channels, application int) (*Encoder, error) {
	var cerr C.int
	st := C.opus_encoder_create(C.opus_int32(sampleRate), C.int(channels), C.int(application), &cerr)
	if cerr != 0 || st == nil {
		return nil, Error(cerr)
	}
	e := &Encoder{st: st}
	runtime.SetFinalizer(e, func(e *Encoder) { _ = e.Close() })
	return e, nil
}

// SetBitrate 设置目标码率（bps）。请求码直接传 OPUS_SET_BITRATE_REQUEST，
// 等价于 OPUS_SET_BITRATE 宏展开后的定参调用。
func (e *Encoder) SetBitrate(bps int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_enc_ctl(e.st, C.OPUS_SET_BITRATE_REQUEST, C.opus_int32(bps)))
}

// SetInBandFEC 开关带内前向纠错。
func (e *Encoder) SetInBandFEC(on bool) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_enc_ctl(e.st, C.OPUS_SET_INBAND_FEC_REQUEST, cBool(on)))
}

// SetDTX 开关非连续传输（静音不发包/发极小包）。
func (e *Encoder) SetDTX(on bool) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_enc_ctl(e.st, C.OPUS_SET_DTX_REQUEST, cBool(on)))
}

// SetPacketLossPerc 告知编码器预期丢包率（%），影响 FEC 强度。
func (e *Encoder) SetPacketLossPerc(pct int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_enc_ctl(e.st, C.OPUS_SET_PACKET_LOSS_PERC_REQUEST, C.opus_int32(pct)))
}

// SetVBR 开关可变码率。
func (e *Encoder) SetVBR(on bool) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_enc_ctl(e.st, C.OPUS_SET_VBR_REQUEST, cBool(on)))
}

// SetComplexity 设置编码复杂度 0–10（0 最快，10 质量最高，默认 9）。
// opus_encoder_ctl 热调安全。
func (e *Encoder) SetComplexity(v int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_enc_ctl(e.st, C.OPUS_SET_COMPLEXITY_REQUEST, C.opus_int32(v)))
}

// Encode 把一帧 PCM（48k 下须为 120/240/480/960/1920/2880 采样）编码到 out。
// 返回写入 out 的字节数。pcm/out 直传指针进 C，无 Go 堆分配。
func (e *Encoder) Encode(pcm []int16, out []byte) (int, error) {
	if e.st == nil {
		return 0, ErrClosed
	}
	if len(pcm) == 0 || len(out) == 0 {
		return 0, ErrEmptyBuffer
	}
	n := C.opus_encode(e.st,
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)),
		(*C.uchar)(unsafe.Pointer(&out[0])), C.opus_int32(len(out)))
	if n < 0 {
		return 0, Error(n)
	}
	return int(n), nil
}

// Close 释放底层编码器。幂等。
func (e *Encoder) Close() error {
	if e.st != nil {
		C.opus_encoder_destroy(e.st)
		e.st = nil
	}
	return nil
}

// Decoder 封装 OpusDecoder。Decode 零分配；Close 后必须弃用。
type Decoder struct {
	st *C.OpusDecoder
}

// NewDecoder 创建解码器。
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	var cerr C.int
	st := C.opus_decoder_create(C.opus_int32(sampleRate), C.int(channels), &cerr)
	if cerr != 0 || st == nil {
		return nil, Error(cerr)
	}
	d := &Decoder{st: st}
	runtime.SetFinalizer(d, func(d *Decoder) { _ = d.Close() })
	return d, nil
}

// Decode 解码一个 Opus 包到 out（采样数），返回采样数。
//
//   - pkt 非空、fec=false：常规解码；
//   - pkt 为空（nil/长度 0）、fec=false：PLC 丢包隐藏，out 长度即要合成的采样数；
//   - pkt 非空、fec=true：用本包的带内 FEC 恢复「上一帧」，out 为上一帧长度。
func (d *Decoder) Decode(pkt []byte, out []int16, fec bool) (int, error) {
	if d.st == nil {
		return 0, ErrClosed
	}
	if len(out) == 0 {
		return 0, ErrEmptyBuffer
	}
	var data *C.uchar
	var n C.opus_int32
	if len(pkt) > 0 {
		data = (*C.uchar)(unsafe.Pointer(&pkt[0]))
		n = C.opus_int32(len(pkt))
	}
	m := C.opus_decode(d.st, data, n,
		(*C.opus_int16)(unsafe.Pointer(&out[0])), C.int(len(out)), cBool(fec))
	if m < 0 {
		return 0, Error(m)
	}
	return int(m), nil
}

// Close 释放底层解码器。幂等。
func (d *Decoder) Close() error {
	if d.st != nil {
		C.opus_decoder_destroy(d.st)
		d.st = nil
	}
	return nil
}

// Version 返回 libopus 版本字符串。
func Version() string {
	return C.GoString(C.opus_get_version_string())
}

// ErrClosed 表示对已 Close 的编解码器发起调用。
var ErrClosed = errors.New("opus codec: encoder/decoder closed")

// ErrEmptyBuffer 表示输入/输出缓冲为空。
var ErrEmptyBuffer = errors.New("opus codec: empty buffer")

func cBool(b bool) C.opus_int32 {
	if b {
		return 1
	}
	return 0
}
