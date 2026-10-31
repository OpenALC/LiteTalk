package push

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// TestNewUnconfiguredReturnsNoop 未配置 = 功能关闭：New 一律回 Noop（零 I/O、
// 零外部连接），NewBroker 拒绝装配（不启 goroutine、不建 channel）。
func TestNewUnconfiguredReturnsNoop(t *testing.T) {
	for name, cfg := range map[string]Config{
		"空配置":        {},
		"显式noop":     {Provider: "noop"},
		"未知provider": {Provider: "xmpp"},
		"fcm缺凭据":     {Provider: "fcm"},
		"apns缺key":   {Provider: "apns", APNSKeyID: "KID", APNSTeamID: "TEAM", APNSTopic: "com.ex.app"},
		"ntfy缺url":   {Provider: "ntfy"},
	} {
		p, impl := New(cfg)
		if _, ok := p.(Noop); !ok {
			t.Fatalf("%s: 应降级 Noop，实际 %s", name, impl)
		}
		if impl != "noop" {
			t.Fatalf("%s: 名称应 noop，实际 %q", name, impl)
		}
	}
	// Noop 不产生 I/O：Push 恒成功且无副作用（接口即无网络字段的空结构）。
	if err := (Noop{}).Push(context.Background(), []Device{{Platform: "ios", Token: "t"}}, Notification{}); err != nil {
		t.Fatalf("Noop.Push 不应失败: %v", err)
	}
	// Noop 不能装配 Broker：保证关闭状态下无 worker/队列。
	if b := NewBroker(BrokerConfig{Pusher: Noop{}, Store: store.NewMem()}); b != nil {
		t.Fatal("Noop 不应装配 Broker（关闭时零副作用）")
	}
	// 显式 provider + 配置齐全才选真实现。
	p, impl := New(Config{Provider: "ntfy", NtfyURL: "https://ntfy.example.com"})
	if _, ok := p.(*Ntfy); !ok || impl != "ntfy" {
		t.Fatalf("ntfy 配置齐全应选 Ntfy，实际 %s", impl)
	}
}

// TestMaskToken push_token 日志打码：只留前 8 位。
func TestMaskToken(t *testing.T) {
	if got := MaskToken("abcdef0123456789"); got != "abcdef01…" {
		t.Fatalf("长 token 打码不符: %q", got)
	}
	// 短 token 固定追加 …，保持统一截断语义。
	if got := MaskToken("abc"); got != "abc…" {
		t.Fatalf("短 token 打码不符: %q", got)
	}
}

// TestAPNsKeyFromFile .p8 凭据支持文件路径（凭据落盘是常见运维形态）。
func TestAPNsKeyFromFile(t *testing.T) {
	_, pemText := mustAPNsKey(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "AuthKey_ABC.p8")
	if err := os.WriteFile(path, []byte(pemText), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := parseAPNsKey(path)
	if err != nil || key == nil {
		t.Fatalf("按路径解析 .p8 失败: %v", err)
	}
	// 非路径也非 PEM 的坏值：保留原值交给解析器报错。
	if _, err := parseAPNsKey("/nonexistent/AuthKey.p8"); err == nil {
		t.Fatal("不存在的私钥路径应报错")
	}
}

// TestFromEnvInlineCredential 内联凭据不经文件读取，原样透传。
func TestFromEnvInlineCredential(t *testing.T) {
	t.Setenv("LITETALK_PUSH_FCM_CREDENTIALS", `{"project_id":"p","client_email":"c@p.iam","private_key":"x"}`)
	cfg := FromEnv()
	if !strings.HasPrefix(cfg.FCMCredentials, "{") {
		t.Fatalf("内联 JSON 应原样透传: %q", cfg.FCMCredentials)
	}
}

// recordingPusher 测试桩：记录投递并通过 channel 通知等待方。
type recordingPusher struct {
	mu     sync.Mutex
	got    []recordedPush
	notify chan struct{}
	fail   int // 前 fail 次 Push 返回错误（测日志打码用）
}

type recordedPush struct {
	Devices []Device
	Notif   Notification
}

func (r *recordingPusher) Push(ctx context.Context, devices []Device, n Notification) error {
	r.mu.Lock()
	r.got = append(r.got, recordedPush{Devices: devices, Notif: n})
	fail := r.fail > 0
	if fail {
		r.fail--
	}
	r.mu.Unlock()
	if r.notify != nil {
		select {
		case r.notify <- struct{}{}:
		default:
		}
	}
	if fail {
		return context.DeadlineExceeded
	}
	return nil
}

func (r *recordingPusher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *recordingPusher) last() recordedPush {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return recordedPush{}
	}
	return r.got[len(r.got)-1]
}

// waitForPush 等待第 n 次投递到达（异步派发的确定性等待），超时 2s。
func waitForPush(t *testing.T, r *recordingPusher, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 次投递超时（实际 %d）", n, r.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// captureLog 捕获 push 实现的日志输出（打码断言用）。
func captureLog(t *testing.T) (*log.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}
