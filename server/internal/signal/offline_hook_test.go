package signal

// offline_hook_test.go 离线检测挂钩（#53）：边沿只在「该用户最后一个连接实例
// 关闭」时回调一次；同设备顶替（半开会话被替换）与多设备存续都不得误报。
// 实例核对语义由 unregister 的 conns[key]==c 保证，这里做行为级回归。

import (
	"sync"
	"testing"
	"time"
)

// recordingOffline 离线边沿记录桩。
type recordingOffline struct {
	mu    sync.Mutex
	edges []string
}

func (r *recordingOffline) OnUserOffline(uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.edges = append(r.edges, uid)
}

func (r *recordingOffline) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.edges...)
}

// waitForOfflineEdge 等待出现第 n 条边沿（cleanup 异步执行）。
func waitForOfflineEdge(t *testing.T, r *recordingOffline, n int) {
	t.Helper()
	deadline := time.Now().Add(wsReadTimeout)
	for {
		if len(r.got()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 条离线边沿超时（实际 %d：%v）", n, len(r.got()), r.got())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOfflineHookLastInstanceOnly 顶替不误报、多设备存续不误报、最后一个实例
// 关闭才报一次。
func TestOfflineHookLastInstanceOnly(t *testing.T) {
	f := newChatFixture(t)
	rec := &recordingOffline{}
	f.hub.SetOfflineNotifier(rec)

	// alice 桌面端首连：在线，无边沿。
	a1 := f.dial("1", "alice")
	if !f.hub.UserOnline("1") {
		t.Fatal("首连后 UserOnline 应为 true")
	}
	if len(rec.got()) != 0 {
		t.Fatalf("首连不应产生边沿: %v", rec.got())
	}

	// 同设备二次拨号 = 顶替（同 sid 半开路径）：旧连接关闭但新连接已注册，
	// 旧连接的 cleanup 不得触发边沿（同一连接实例核对）。
	a2 := f.dial("1", "alice2")
	time.Sleep(100 * time.Millisecond) // 给旧连接 cleanup 留失败窗口
	if edges := rec.got(); len(edges) != 0 {
		t.Fatalf("顶替不得触发离线边沿: %v", edges)
	}
	if !f.hub.UserOnline("1") {
		t.Fatal("顶替后仍应在线")
	}
	_ = a1 // a1 已被顶替，引用仅为显式说明

	// 多设备：mobile 再连一条，关桌面端 → 仍有连接，无边沿。
	aM := f.dial("1:mobile", "aliceM")
	a2.conn.CloseNow()
	time.Sleep(100 * time.Millisecond)
	if edges := rec.got(); len(edges) != 0 {
		t.Fatalf("多设备存续关闭其一不得触发边沿: %v", edges)
	}

	// 关掉最后一个实例 → 恰一条边沿，且 UserOnline 先行翻 false。
	aM.conn.CloseNow()
	waitForOfflineEdge(t, rec, 1)
	deadline := time.Now().Add(wsReadTimeout)
	for f.hub.UserOnline("1") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.hub.UserOnline("1") {
		t.Fatal("全部关闭后 UserOnline 应为 false")
	}
	if edges := rec.got(); len(edges) != 1 || edges[0] != "1" {
		t.Fatalf("应恰一条 uid=1 的边沿: %v", edges)
	}

	// bob 全程未连接：不产生任何边沿。
	aB := f.dial("2", "bob")
	aB.conn.CloseNow()
	waitForOfflineEdge(t, rec, 2)
	if edges := rec.got(); len(edges) != 2 || edges[1] != "2" {
		t.Fatalf("bob 关闭应产生 uid=2 边沿: %v", edges)
	}
}
