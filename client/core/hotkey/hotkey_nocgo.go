//go:build (linux || darwin) && !cgo

package hotkey

// 关闭 CGO 时 linux / darwin 的 fallback stub。
//
// 两个平台的实现都是 cgo 文件，CGO_ENABLED=0 下会被 Go 构建系统自动排除，
// 于是这两个平台就没有 source 了。这与 codec/opus_stub.go 用
// `!windows && (!linux || !cgo)` 照顾的是同一件事——那边靠 stub 的标签显式
// 包含 !cgo，这边因为 fallback 的标签已收窄到排除 linux/darwin，需要单独一份。
//
// 客户端本身离了 cgo 建不起来（libopus / malgo / GTK），这条路径主要是让
// `go vet`、`go build` 在关闭 CGO 的环境里不报 undefined，而不是声称支持。
type nocgoSource struct{}

func newPlatformSource() source { return nocgoSource{} }

func (nocgoSource) available() bool { return false }

func (nocgoSource) start(Settings, chan<- Event) error { return ErrUnsupportedPlatform }

func (nocgoSource) stop() {}

func (nocgoSource) update(Settings) {}
