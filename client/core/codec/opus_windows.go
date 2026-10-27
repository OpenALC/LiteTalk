//go:build windows

// Package codec 是 libopus 的自研 cgo 薄封装。
//
// 头文件与静态库均在仓库内 client/third_party/opus/，全部经 ${SRCDIR}
// 相对路径引用，任何机器上 go build 免 pkg-config、免额外环境变量（CC 除外）。
// 链接为静态方式，产物仅依赖 KERNEL32 + UCRT 系统 DLL。
package codec

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/opus/include/opus
#cgo LDFLAGS: -Wl,-Bstatic -L${SRCDIR}/../../third_party/opus/lib -Wl,--start-group -lopus -lm -Wl,--end-group -Wl,-Bdynamic -static-libgcc

#include <opus.h>

// CTL 宏（OPUS_SET_BITRATE(x) 等）包一层静态函数，避免 cgo 直调可变参数宏。
static int lt_set_bitrate(OpusEncoder *e, opus_int32 v) { return opus_encoder_ctl(e, OPUS_SET_BITRATE(v)); }
static int lt_set_inband_fec(OpusEncoder *e, int v)       { return opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(v)); }
static int lt_set_dtx(OpusEncoder *e, int v)              { return opus_encoder_ctl(e, OPUS_SET_DTX(v)); }
static int lt_set_plp(OpusEncoder *e, int v)              { return opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(v)); }
static int lt_set_vbr(OpusEncoder *e, int v)              { return opus_encoder_ctl(e, OPUS_SET_VBR(v)); }
static int lt_set_complexity(OpusEncoder *e, int v)       { return opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(v)); }
static int lt_set_dred(OpusEncoder *e, int v)             { return opus_encoder_ctl(e, OPUS_SET_DRED_DURATION(v)); }
static int lt_get_plp(OpusEncoder *e, opus_int32 *v)      { return opus_encoder_ctl(e, OPUS_GET_PACKET_LOSS_PERC(v)); }
static int lt_get_dred(OpusEncoder *e, opus_int32 *v)     { return opus_encoder_ctl(e, OPUS_GET_DRED_DURATION(v)); }
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

// ErrUnsupportedPlatform 在非 Windows 平台由 stub 返回；Windows 实现保留同名
// 变量以维持跨平台 API 一致（调用方可用 errors.Is 判断）。
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

// SetBitrate 设置目标码率（bps）。
func (e *Encoder) SetBitrate(bps int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_bitrate(e.st, C.opus_int32(bps)))
}

// SetInBandFEC 开关带内前向纠错。
func (e *Encoder) SetInBandFEC(on bool) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_inband_fec(e.st, cBool(on)))
}

// SetDTX 开关非连续传输（静音不发包/发极小包）。
func (e *Encoder) SetDTX(on bool) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_dtx(e.st, cBool(on)))
}

// SetPacketLossPerc 告知编码器预期丢包率（%），影响 FEC 强度。
func (e *Encoder) SetPacketLossPerc(pct int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_plp(e.st, C.int(pct)))
}

// SetVBR 开关可变码率。
func (e *Encoder) SetVBR(on bool) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_vbr(e.st, cBool(on)))
}

// SetComplexity 设置编码复杂度 0–10（v0.1.2；0 最快，10 质量最高，默认 9）。
// opus_encoder_ctl 热调安全。
func (e *Encoder) SetComplexity(v int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_complexity(e.st, C.int(v)))
}

// SetDRED 设置深度冗余（DRED）的最大帧数（10ms/帧）：0 关闭，20 = 200ms。
// 注意 libopus 只在预期丢包率足够高时才真正分配冗余（见 audio 包的 PLP 联动），
// 低 PLP 下打开也可能不产出 DRED 数据。旧库/未启用 DRED 的构建返回错误。
func (e *Encoder) SetDRED(frames int) error {
	if e.st == nil {
		return ErrClosed
	}
	return ctlError(C.lt_set_dred(e.st, C.int(frames)))
}

// DREDDuration 读取当前 DRED 最大帧数（0=关闭）。诊断/测试用。
func (e *Encoder) DREDDuration() (int, error) {
	if e.st == nil {
		return 0, ErrClosed
	}
	var v C.opus_int32
	if err := ctlError(C.lt_get_dred(e.st, &v)); err != nil {
		return 0, err
	}
	return int(v), nil
}

// PacketLossPerc 读取编码器当前预期丢包率（%）。诊断/测试用。
func (e *Encoder) PacketLossPerc() (int, error) {
	if e.st == nil {
		return 0, ErrClosed
	}
	var v C.opus_int32
	if err := ctlError(C.lt_get_plp(e.st, &v)); err != nil {
		return 0, err
	}
	return int(v), nil
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

// DREDDecoder 封装 Opus 深度冗余（DRED）解码器，复用一块 OpusDRED 状态。
// 仅限单个 goroutine 使用（丢帧路径），Close 后必须弃用。
type DREDDecoder struct {
	dec   *C.OpusDREDDecoder
	state *C.OpusDRED
}

// NewDREDDecoder 创建 DRED 解码器。库未启用 DRED 时返回错误。
func NewDREDDecoder() (*DREDDecoder, error) {
	var cerr C.int
	dec := C.opus_dred_decoder_create(&cerr)
	if cerr != 0 || dec == nil {
		return nil, Error(cerr)
	}
	var aerr C.int
	st := C.opus_dred_alloc(&aerr)
	if aerr != 0 || st == nil {
		C.opus_dred_decoder_destroy(dec)
		if aerr == 0 {
			aerr = C.OPUS_ALLOC_FAIL
		}
		return nil, Error(aerr)
	}
	d := &DREDDecoder{dec: dec, state: st}
	runtime.SetFinalizer(d, func(d *DREDDecoder) { _ = d.Close() })
	return d, nil
}

// Recover 用后继包 pkt 里的 DRED 冗余重建「紧邻其前」的一帧，写入 out。
//
// frameSamples 是帧长（48k 采样数：10ms=480 / 20ms=960）；sampleRate 只用于
// 解释 max_dred_samples 的单位（本项目固定 48k）。ok=false 表示该包不含可用
// 冗余，调用方应回落 PLC。
//
// 陷阱：底层 OpusDecoder 必须先正常解码过丢帧之前的包（warm 状态），否则
// DRED 输出静音；DRED 数据来自「后继包」，不是丢失包本身。
func (d *DREDDecoder) Recover(dec *Decoder, pkt []byte, out []int16, frameSamples, sampleRate int) (n int, ok bool, err error) {
	if d.dec == nil || d.state == nil {
		return 0, false, ErrClosed
	}
	if dec == nil || dec.st == nil {
		return 0, false, ErrClosed
	}
	if len(pkt) == 0 || frameSamples <= 0 || sampleRate <= 0 || len(out) < frameSamples {
		return 0, false, ErrEmptyBuffer
	}
	var dredEnd C.int
	off := C.opus_dred_parse(d.dec, d.state,
		(*C.uchar)(unsafe.Pointer(&pkt[0])), C.opus_int32(len(pkt)),
		C.opus_int32(frameSamples), C.opus_int32(sampleRate), &dredEnd, 0)
	if off == 0 {
		return 0, false, nil // 包里没有 DRED
	}
	if off < 0 {
		return 0, false, Error(off)
	}
	// defer_processing=0 时也需显式 process：库只有在 process 后才把 DRED
	// 状态整理成 opus_decoder_dred_decode 可用的形式（src/dst 可为同一块）。
	if r := C.opus_dred_process(d.dec, d.state, d.state); r < 0 {
		return 0, false, Error(r)
	}
	m := C.opus_decoder_dred_decode(dec.st, d.state, C.opus_int32(frameSamples),
		(*C.opus_int16)(unsafe.Pointer(&out[0])), C.int(frameSamples))
	if m < 0 {
		return 0, false, Error(m)
	}
	if m == 0 {
		return 0, false, nil // 冗余窗口不足：回落 PLC
	}
	return int(m), true, nil
}

// Close 释放底层 DRED 解码器与状态。幂等。
func (d *DREDDecoder) Close() error {
	if d.state != nil {
		C.opus_dred_free(d.state)
		d.state = nil
	}
	if d.dec != nil {
		C.opus_dred_decoder_destroy(d.dec)
		d.dec = nil
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

func cBool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}
