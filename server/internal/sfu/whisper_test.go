package sfu

import (
	"testing"
	"time"
)

// #45 同频道悄悄话：转发优先级 serverMuted > whisper 白名单 > fwdActive。

// TestWhisperFiltersToTargets 只向白名单成员转发；清除后恢复全房转发。
// 非目标成员在悄悄话期间一个包都不应收到。
func TestWhisperFiltersToTargets(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")
	carol := newTestClient(t, s, "c1", "carol")

	if !s.SetWhisperTargets("c1", "alice", "alice", []string{"bob"}, true) {
		t.Fatal("SetWhisperTargets 应命中在房成员")
	}
	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "whisper")

	if bob.waitFor(sent, 10*time.Second) == nil {
		t.Fatal("悄悄话目标 bob 未收到 RTP")
	}
	time.Sleep(500 * time.Millisecond) // 给非目标足够时间暴露误转发
	if n := carol.countMatching(sent); n != 0 {
		t.Fatalf("非目标 carol 收到 %d 个悄悄话包", n)
	}

	// 空目标 = 取消：恢复普通全房转发。
	if !s.SetWhisperTargets("c1", "alice", "alice", nil, false) {
		t.Fatal("清除悄悄话应命中")
	}
	sentAfter := newSentSet()
	go alice.sendLoop(stop, sentAfter, "normal")
	if carol.waitFor(sentAfter, 10*time.Second) == nil {
		t.Fatal("取消悄悄话后 carol 未恢复收到 RTP")
	}
}

// TestWhisperBypassesActiveGate 活跃门把源挡下时，悄悄话仍必须送达目标——
// top-N 是带宽决策，不能吞掉悄悄话。
func TestWhisperBypassesActiveGate(t *testing.T) {
	s := newTestSFUCfg(t, Config{
		UDPPort: 0, NAT1To1IP: "127.0.0.1", MaxMembers: 10,
		ActiveN: 1, ActiveHoldMS: 100,
	})
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")
	newTestClient(t, s, "c1", "carol")

	// alice 记为轻声、bob/carol 为响者：ActiveN=1 下 alice 会跌出 top-1。
	// sendLoop 的包不带 RFC6464 扩展，forward 不会覆盖这里写入的电平。
	alicePeer := s.findPeer("c1", "alice")
	if alicePeer == nil {
		t.Fatal("alice peer 不存在")
	}
	alicePeer.level.Store(60)
	s.findPeer("c1", "bob").level.Store(10)
	s.findPeer("c1", "carol").level.Store(10)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alicePeer.fwdActive.Load() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alicePeer.fwdActive.Load() {
		t.Fatal("前置条件失败：alice 应被活跃门关闭")
	}

	if !s.SetWhisperTargets("c1", "alice", "alice", []string{"bob"}, true) {
		t.Fatal("SetWhisperTargets 应命中")
	}
	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "bypass")
	if bob.waitFor(sent, 10*time.Second) == nil {
		t.Fatal("悄悄话未绕过活跃门：目标 bob 收不到 RTP")
	}
}

// TestServerMutedBeatsWhisper 服务端强制闭麦的优先级高于悄悄话：仍整包丢弃。
func TestServerMutedBeatsWhisper(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")

	if !s.SetWhisperTargets("c1", "alice", "alice", []string{"bob"}, true) {
		t.Fatal("SetWhisperTargets 应命中")
	}
	if !s.SetServerMuted("c1", "alice", true) {
		t.Fatal("SetServerMuted 应命中")
	}
	base := s.serverMutedDropped.Load()

	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "muted-whisper")
	time.Sleep(500 * time.Millisecond)

	if n := bob.countMatching(sent); n != 0 {
		t.Fatalf("服务端闭麦期间悄悄话仍被转发 %d 包", n)
	}
	if s.serverMutedDropped.Load() == base {
		t.Fatal("serverMuted 丢弃计数应增长")
	}
}

// TestWhisperSwitchTargets 切换目标：旧目标停止收到、新目标开始收到。
func TestWhisperSwitchTargets(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")
	carol := newTestClient(t, s, "c1", "carol")

	if !s.SetWhisperTargets("c1", "alice", "alice", []string{"bob"}, true) {
		t.Fatal("SetWhisperTargets 应命中")
	}
	stop := make(chan struct{})
	defer close(stop)
	sentBob := newSentSet()
	go alice.sendLoop(stop, sentBob, "to-bob")
	if bob.waitFor(sentBob, 10*time.Second) == nil {
		t.Fatal("切换前 bob 未收到 RTP")
	}

	// 切换到 carol：用新标记，避免切换瞬间的在途包干扰计数。
	if !s.SetWhisperTargets("c1", "alice", "alice", []string{"carol"}, true) {
		t.Fatal("切换目标应命中")
	}
	sentCarol := newSentSet()
	go alice.sendLoop(stop, sentCarol, "to-carol")
	if carol.waitFor(sentCarol, 10*time.Second) == nil {
		t.Fatal("切换后 carol 未收到 RTP")
	}
	time.Sleep(300 * time.Millisecond)
	if n := bob.countMatching(sentCarol); n != 0 {
		t.Fatalf("切换后旧目标 bob 仍收到 %d 个包", n)
	}
}

// TestWhisperConnIdentity 归属核对：错误 connID 不得改写；离房重入的新实例
// 不继承悄悄话状态。
func TestWhisperConnIdentity(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	_ = alice

	if s.SetWhisperTargets("c1", "alice", "stale-conn", []string{"bob"}, true) {
		t.Fatal("connID 不符时应拒绝")
	}
	if p := s.findPeer("c1", "alice"); p == nil || p.whisperTargets.Load() != nil {
		t.Fatal("被拒绝的调用不得改写悄悄话状态")
	}
	if !s.SetWhisperTargets("c1", "alice", "alice", []string{"bob"}, true) {
		t.Fatal("正确 connID 应命中")
	}

	s.LeaveIf("c1", "alice", "alice")
	alice2 := newTestClient(t, s, "c1", "alice")
	_ = alice2
	if p := s.findPeer("c1", "alice"); p == nil || p.whisperTargets.Load() != nil {
		t.Fatal("重入的新 peer 不应继承悄悄话目标")
	}
}

// TestWhisperActiveEmptyTargets 目标全部离房/被过滤掉时保持悄悄话态：
// 谁都不发，绝不回落成全房广播（隐私边界）。
func TestWhisperActiveEmptyTargets(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")
	_ = bob

	// active=true + 空集合：悄悄话处于「暂无接收者」状态。
	if !s.SetWhisperTargets("c1", "alice", "alice", nil, true) {
		t.Fatal("SetWhisperTargets 应命中")
	}
	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "empty-whisper")
	time.Sleep(500 * time.Millisecond)
	if n := bob.countMatching(sent); n != 0 {
		t.Fatalf("目标为空时不得回落全房广播，bob 收到 %d 包", n)
	}

	// 显式取消后恢复普通转发。
	if !s.SetWhisperTargets("c1", "alice", "alice", nil, false) {
		t.Fatal("取消悄悄话应命中")
	}
	sentNormal := newSentSet()
	go alice.sendLoop(stop, sentNormal, "normal")
	if bob.waitFor(sentNormal, 10*time.Second) == nil {
		t.Fatal("取消悄悄话后应恢复全房转发")
	}
}

// TestWhisperConcurrentSet 并发设置与转发（-race 下验证快照读写安全）。
func TestWhisperConcurrentSet(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	_ = newTestClient(t, s, "c1", "bob")

	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "race")

	done := make(chan struct{})
	go func() {
		defer close(done)
		targets := [][]string{{"bob"}, nil, {"bob", "carol"}, {}}
		for i := 0; i < 200; i++ {
			if !s.SetWhisperTargets("c1", "alice", "alice", targets[i%len(targets)], true) {
				t.Errorf("SetWhisperTargets 应命中（第 %d 次）", i)
				return
			}
		}
	}()
	<-done
	time.Sleep(100 * time.Millisecond)
}
