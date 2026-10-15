//go:build !windows

// Package codec 的非 Windows stub。
//
// 本阶段 client/third_party 只有 Windows x86_64 预编译 libopus，
// 其他平台返回 ErrUnsupportedPlatform，保证 Linux/macOS 上
// go vet / go test / go build 不炸（M4 打包前再补各平台预编译库）。
package codec

import "errors"

// 与 opus_windows.go 保持一致的常量与 API 面。
const (
	AppVoIP     = 2048
	AppAudio    = 2049
	AppLowDelay = 2051
)

// ErrUnsupportedPlatform 表示当前平台没有可用的 libopus 实现。
var ErrUnsupportedPlatform = errors.New("opus codec: unsupported platform")

// Supported 报告当前平台是否有可用的 libopus 实现。
func Supported() bool { return false }

// Error 是 libopus 返回的负值错误码（stub 下不会产生）。
type Error int

func (e Error) Error() string { return "opus: unsupported platform" }

// ErrClosed 表示对已 Close 的编解码器发起调用。
var ErrClosed = errors.New("opus codec: encoder/decoder closed")

// ErrEmptyBuffer 表示输入/输出缓冲为空。
var ErrEmptyBuffer = errors.New("opus codec: empty buffer")

// Encoder stub。
type Encoder struct{}

// NewEncoder stub：永远返回 ErrUnsupportedPlatform。
func NewEncoder(sampleRate, channels, application int) (*Encoder, error) {
	return nil, ErrUnsupportedPlatform
}

// SetBitrate stub。
func (e *Encoder) SetBitrate(bps int) error { return ErrUnsupportedPlatform }

// SetInBandFEC stub。
func (e *Encoder) SetInBandFEC(on bool) error { return ErrUnsupportedPlatform }

// SetDTX stub。
func (e *Encoder) SetDTX(on bool) error { return ErrUnsupportedPlatform }

// SetPacketLossPerc stub。
func (e *Encoder) SetPacketLossPerc(pct int) error { return ErrUnsupportedPlatform }

// SetVBR stub。
func (e *Encoder) SetVBR(on bool) error { return ErrUnsupportedPlatform }

// SetComplexity 非 Windows stub。
func (e *Encoder) SetComplexity(v int) error { return ErrUnsupportedPlatform }

// Encode stub。
func (e *Encoder) Encode(pcm []int16, out []byte) (int, error) {
	return 0, ErrUnsupportedPlatform
}

// Close stub。
func (e *Encoder) Close() error { return nil }

// Decoder stub。
type Decoder struct{}

// NewDecoder stub：永远返回 ErrUnsupportedPlatform。
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	return nil, ErrUnsupportedPlatform
}

// Decode stub。
func (d *Decoder) Decode(pkt []byte, out []int16, fec bool) (int, error) {
	return 0, ErrUnsupportedPlatform
}

// Close stub。
func (d *Decoder) Close() error { return nil }

// Version stub。
func Version() string { return "unsupported" }
