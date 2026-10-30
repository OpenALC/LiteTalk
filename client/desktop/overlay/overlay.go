// Package overlay 实现游戏内悬浮层：Windows 原生分层窗口（不开 WebView），
// 显示当前正在说话的语音房间成员。
//
// 仅 Windows 有实现；其余平台 Supported 返回 false、Start 返回
// ErrUnsupportedPlatform，设置页由前端禁用开关并提示。
//
// 边界：不注入游戏进程、不保证在独占全屏（DX exclusive）游戏之上可见——
// 无边框/窗口全屏可以覆盖；也不保证兼容所有反作弊。
package overlay

import (
	"errors"
	"log"
	"sort"
	"sync"
	"unicode/utf16"
)

// MaxSpeakers 悬浮层最多展示的成员数。
const MaxSpeakers = 6

// Speaker 悬浮层需要的成员状态（App 侧从 voice.PeerView 投影）。
type Speaker struct {
	UID      string
	Nickname string
	Speaking bool
	Mute     bool
	Deafen   bool
	Priority bool
}

// Snapshot 返回当前房间成员快照；由悬浮层线程每 100ms 调用一次。
// 实现必须并发安全且快速返回，不得阻塞在 I/O 上。
type Snapshot func() []Speaker

// ErrUnsupportedPlatform 表示当前平台没有原生悬浮层实现。
var ErrUnsupportedPlatform = errors.New("当前平台不支持游戏内悬浮层")

// 平台实现注入点：正式构建取 overlay_windows.go / overlay_other.go，
// 测试替换成桩以覆盖生命周期逻辑（不创建真实窗口）。
var (
	platformSupported = supportedPlatform
	platformStart     = startPlatform
	platformStop      = stopPlatform
)

var (
	mu      sync.Mutex
	started bool
)

// Supported 报告当前平台是否提供原生悬浮层。
func Supported() bool { return platformSupported() }

// Start 启动悬浮层，snapshot 由悬浮层线程周期调用。重复调用无副作用；
// 平台不支持返回 ErrUnsupportedPlatform，窗口创建失败返回底层错误。
func Start(snapshot Snapshot) error {
	if snapshot == nil {
		return errors.New("overlay: snapshot 不能为空")
	}
	mu.Lock()
	defer mu.Unlock()
	if started {
		return nil
	}
	if !platformSupported() {
		return ErrUnsupportedPlatform
	}
	if err := platformStart(snapshot); err != nil {
		return err
	}
	started = true
	return nil
}

// Stop 关闭悬浮层并等待窗口线程退出。重复调用或未启动时无副作用。
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if !started {
		return
	}
	started = false
	platformStop()
}

// identityColors 与前端 SoundOrb.vue 的身份色一致（hash(uid)%4 的四个品牌邻近色），
// 以 Win32 COLORREF（0x00BBGGRR）形式保存。
var identityColors = [4]uint32{
	rgb(0x4F, 0xA4, 0xF3),
	rgb(0x3D, 0xBB, 0xC4),
	rgb(0x7C, 0x8C, 0xF8),
	rgb(0x3F, 0xB5, 0x7F),
}

// rgb 把 0xRRGGBB 转成 COLORREF。非 Windows 平台不使用，但测试与平台实现
// 共享同一份定义，避免颜色两处真相。
func rgb(r, g, b byte) uint32 {
	return uint32(r) | uint32(g)<<8 | uint32(b)<<16
}

// IdentityColorIndex 复刻前端 SoundOrb.vue 的 hashId：按 UTF-16 码元迭代、
// 每步 int32 回绕（等价 JS 的 |0），取绝对值后对 4 取模。
// 必须与前端逐位一致，否则同一个人在悬浮层和语音房会显示两种颜色。
func IdentityColorIndex(uid string) int {
	var h int32
	for _, u := range utf16.Encode([]rune(uid)) {
		h = h*31 + int32(u)
	}
	v := int64(h)
	if v < 0 {
		v = -v
	}
	return int(v % 4)
}

// compose 过滤非说话者、按 UID 去重，优先发言者在前，其余按昵称（再按 UID）
// 排序，最后截断到 MaxSpeakers。返回的切片归悬浮层线程所有。
func compose(in []Speaker) []Speaker {
	out := make([]Speaker, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if !s.Speaking {
			continue
		}
		if s.UID != "" {
			if _, dup := seen[s.UID]; dup {
				continue
			}
			seen[s.UID] = struct{}{}
		}
		if s.Nickname == "" {
			s.Nickname = s.UID
		}
		if s.Nickname == "" {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority
		}
		if out[i].Nickname != out[j].Nickname {
			return out[i].Nickname < out[j].Nickname
		}
		return out[i].UID < out[j].UID
	})
	if len(out) > MaxSpeakers {
		out = out[:MaxSpeakers]
	}
	return out
}

// sameSpeakers 判断两份快照是否一致（含状态位），一致时跳过重绘。
func sameSpeakers(a, b []Speaker) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// safeSnapshot 隔离快照回调：回调在悬浮层窗口线程上执行，panic 会杀掉整个
// 进程（而不是只让悬浮层消失），这里兜住并按无说话者处理。
func safeSnapshot(s Snapshot) (out []Speaker) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("overlay: snapshot panic: %v", r)
			out = nil
		}
	}()
	return s()
}
