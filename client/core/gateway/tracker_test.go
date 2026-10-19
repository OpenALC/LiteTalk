package gateway

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/signal"
)

func env(seq int64) signal.Envelope {
	return signal.Envelope{Type: signal.TypeChatMessage, Data: json.RawMessage(`{}`), TS: seq, Seq: seq}
}

func types(out []signal.Envelope) []int64 {
	seqs := make([]int64, len(out))
	for i, e := range out {
		seqs[i] = e.Seq
	}
	return seqs
}

func eq(a, b []int64) bool {
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

// TestTrackerOrderingDedup 乱序缓冲与去重：补发与实时交错到达时，
// 只有连续段被投递；同 seq 重复只投一次（环补发与实时投递重叠）。
func TestTrackerOrderingDedup(t *testing.T) {
	tr := New()
	// 实时抢跑（seq 5 先到）：缓冲不投。
	if out := tr.Gate(env(5)); len(out) != 0 {
		t.Fatalf("空洞期不应投递: %v", types(out))
	}
	// 补发 3,4 到达：3 也不投（空洞在 1..2），4 也不投。
	if out := tr.Gate(env(3)); len(out) != 0 {
		t.Fatalf("seq3 不应投递: %v", types(out))
	}
	// 1,2 补齐：连续段 1..5 一次释放。
	out := tr.Gate(env(1))
	out = append(out, tr.Gate(env(2))...)
	out = append(out, tr.Gate(env(4))...) // 4 是重复补发
	if !eq(types(out), []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("连续段 = %v, want [1 2 3 4 5]", types(out))
	}
	if tr.LastSeq() != 5 {
		t.Fatalf("LastSeq = %d, want 5", tr.LastSeq())
	}
	// 已投递过的 seq 再来（补发重叠）：去重不投。
	if out := tr.Gate(env(5)); len(out) != 0 {
		t.Fatalf("重复 seq 应去重: %v", types(out))
	}
	// 续流：6 直接投。
	if out := tr.Gate(env(6)); !eq(types(out), []int64{6}) {
		t.Fatalf("续流 = %v", types(out))
	}
}

// TestTrackerPassThrough 无 seq 消息（语音面/连接控制）直通且不影响序号基准。
func TestTrackerPassThrough(t *testing.T) {
	tr := New()
	plain := signal.Envelope{Type: signal.TypeVoiceState, Data: json.RawMessage(`{}`)}
	out := tr.Gate(plain)
	if len(out) != 1 || out[0].Type != signal.TypeVoiceState {
		t.Fatalf("无 seq 消息未直通: %+v", out)
	}
	if tr.LastSeq() != 0 {
		t.Fatalf("直通消息不应影响序号基准: %d", tr.LastSeq())
	}
}

// TestTrackerResumeDecision server.hello 决策：首次只记录；同会话返回 resume 载荷；
// 旧服务端（版本<2）与无会话令牌不续传。
func TestTrackerResumeDecision(t *testing.T) {
	tr := New()
	if _, ok := tr.OnServerHello("S1", 2); ok {
		t.Fatal("首次协商不应续传")
	}
	if tr.SessionID() != "S1" {
		t.Fatalf("SessionID = %q", tr.SessionID())
	}
	tr.Gate(env(1))
	tr.Gate(env(2))
	d, ok := tr.OnServerHello("S1", 2)
	if !ok || d.SessionID != "S1" || d.LastSeq != 2 {
		t.Fatalf("续传载荷 = (%+v,%v), want {S1 2} true", d, ok)
	}
	// 协议 v1（老服务端）不续传。
	if _, ok := tr.OnServerHello("S1", 1); ok {
		t.Fatal("v1 不应续传")
	}
	// 无会话（旧令牌）不续传。
	tr2 := New()
	tr2.OnServerHello("", 2)
	if _, ok := tr2.OnServerHello("", 2); ok {
		t.Fatal("无会话不应续传")
	}
}

// TestTrackerInvalidSession invalid_session：在途事件按序投递、序号基准清零
// （服务端环可能从 1 重计）、异步触发全量补拉。
func TestTrackerInvalidSession(t *testing.T) {
	tr := New()
	resync := make(chan string, 1)
	tr.OnResync(func(reason string) { resync <- reason })

	if out := tr.Gate(env(5)); len(out) != 0 {
		t.Fatalf("空洞期不应投递: %v", types(out))
	}
	// invalid_session 由本包消化，不回投；在途的 5 先放行（消费者幂等）。
	out := tr.Gate(signal.Envelope{Type: signal.TypeGatewayInvalidSession})
	if !eq(types(out), []int64{5}) {
		t.Fatalf("在途事件 = %v, want [5]", types(out))
	}
	select {
	case <-resync:
	case <-time.After(2 * time.Second):
		t.Fatal("全量补拉回调未触发")
	}
	if tr.LastSeq() != 0 {
		t.Fatalf("续传失败后序号基准应清零: %d", tr.LastSeq())
	}
	// 新流从 1 重计也能投递（基准已清零）。
	if out := tr.Gate(env(1)); !eq(types(out), []int64{1}) {
		t.Fatalf("重置后新流 = %v", types(out))
	}
}

// TestTrackerPendingOverflow 缺失段永不补齐：缓冲到上限转全量补拉，不再空等。
func TestTrackerPendingOverflow(t *testing.T) {
	tr := New()
	resync := make(chan string, 1)
	tr.OnResync(func(reason string) { resync <- reason })
	var flushed []signal.Envelope
	for i := int64(1); i <= maxPending+2; i++ {
		if out := tr.Gate(env(i + 1000)); len(out) > 0 { // 与基准（期望 1）有空洞
			flushed = out
		}
	}
	if len(flushed) == 0 {
		t.Fatal("溢出时应把在途事件放行")
	}
	select {
	case reason := <-resync:
		if reason != "pending_overflow" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("溢出后全量补拉回调未触发")
	}
	if tr.LastSeq() != 0 {
		t.Fatalf("溢出后序号基准应清零: %d", tr.LastSeq())
	}
}
