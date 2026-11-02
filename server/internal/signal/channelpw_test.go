package signal

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
)

// #102 频道密码 bcrypt 放大：失败窗口（成功不计入、滑动窗）与全局并发闸。

// countingPwCheck 包一层真实校验并计数——用来断言「限流期间一次 bcrypt 都不跑」。
type countingPwCheck struct {
	n atomic.Int64
}

func (c *countingPwCheck) check(hash, plain string) bool {
	c.n.Add(1)
	return auth.CheckPassword(hash, plain)
}

// TestChannelPwFailLimit 连续错误触发限流：前 5 次都真的跑了校验并回
// bad_password，第 6 次直接 rate_limited 且不再调用校验函数。
func TestChannelPwFailLimit(t *testing.T) {
	f := newChatFixture(t)
	setVchPassword(t, f, "right-pw")
	vchID := strconv.FormatInt(f.vch.ID, 10)
	counter := &countingPwCheck{}
	f.hub.pwCheck = counter.check

	b := f.dial("2", "bob")
	for i := 0; i < channelPwFailLimit; i++ {
		b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "wrong"})
		wantError(t, b, ErrCodeBadPassword)
	}
	if got := counter.n.Load(); got != channelPwFailLimit {
		t.Fatalf("限流前的校验次数 = %d，want %d", got, channelPwFailLimit)
	}

	// 第 6 次：即使密码正确也先被限流闸拦下，不跑 bcrypt。
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "right-pw"})
	wantError(t, b, ErrCodeRateLimited)
	if got := counter.n.Load(); got != channelPwFailLimit {
		t.Fatalf("限流期间不得调用 bcrypt：校验次数 = %d，want %d", got, channelPwFailLimit)
	}
}

// TestChannelPwSuccessNotCounted 成功校验不计入失败计数并清空历史：
// 输错几次后输对能进，之后再错仍然按 bad_password 处理而不是被自己的
// 历史错误锁死。
func TestChannelPwSuccessNotCounted(t *testing.T) {
	f := newChatFixture(t)
	setVchPassword(t, f, "right-pw")
	vchID := strconv.FormatInt(f.vch.ID, 10)

	b := f.dial("2", "bob")
	for i := 0; i < channelPwFailLimit-1; i++ {
		b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "wrong"})
		wantError(t, b, ErrCodeBadPassword)
	}
	// 还差一次就限流：此时输对必须放行，并把计数清零。
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "right-pw"})
	b.recvUntil(TypeChannelJoined)
	b.send(TypeChannelLeave, ChannelLeaveData{})

	// 清零之后又有完整的失败配额：第一次仍回 bad_password（不是 rate_limited）。
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "wrong"})
	wantError(t, b, ErrCodeBadPassword)
}

// TestChannelPwFailWindowSlides 失败计数按窗口滑动：窗口外的旧失败被剪掉，
// 不能永久累积成封锁。
func TestChannelPwFailWindowSlides(t *testing.T) {
	f := newChatFixture(t)
	h := f.hub
	key := makePwFailKey("2", "77")
	base := time.Now()

	for i := 0; i < channelPwFailLimit; i++ {
		if !h.pwAttemptAllowed(key, base) {
			t.Fatalf("第 %d 次判定就该放行", i+1)
		}
		h.pwRecordFailure(key, base)
	}
	if h.pwAttemptAllowed(key, base) {
		t.Fatal("窗口内第 6 次应被限流")
	}
	// 恰好越过窗口：旧失败全部过期，重新放行。
	if !h.pwAttemptAllowed(key, base.Add(channelPwFailWindow+time.Second)) {
		t.Fatal("窗口滑过后应重新放行")
	}
	// 空键已被剪掉（不留空记录）。
	h.pwMu.Lock()
	_, kept := h.pwFails[key]
	h.pwMu.Unlock()
	if kept {
		t.Fatal("窗口过期后应删除空键，避免 map 无界增长")
	}
}

// TestChannelPwFailLimitPerUser 限流按 uid+频道隔离：一个账号打满不会牵连别人。
func TestChannelPwFailLimitPerUser(t *testing.T) {
	f := newChatFixture(t)
	setVchPassword(t, f, "right-pw")
	vchID := strconv.FormatInt(f.vch.ID, 10)

	b := f.dial("2", "bob")
	for i := 0; i < channelPwFailLimit; i++ {
		b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "wrong"})
		wantError(t, b, ErrCodeBadPassword)
	}
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "right-pw"})
	wantError(t, b, ErrCodeRateLimited)

	// 另一个账号（mallory=9）自己的配额是满的，带对密码照常进房。
	m := f.dial("9", "mallory")
	m.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "right-pw"})
	m.recvUntil(TypeChannelJoined)
}

// TestChannelPwConcurrencyGate 并发信号量：拿不到许可直接拒绝且不跑校验，
// 保证大量账号同时猜密码时 bcrypt 的并发数有上限。
func TestChannelPwConcurrencyGate(t *testing.T) {
	f := newChatFixture(t)
	var calls atomic.Int64
	f.hub.pwCheck = func(hash, plain string) bool {
		calls.Add(1)
		return plain == "right-pw"
	}
	f.hub.pwWait = 20 * time.Millisecond

	// 直接占满全部许可：模拟 channelPwConcurrency 路校验正在同时跑。
	for i := 0; i < channelPwConcurrency; i++ {
		f.hub.pwSem <- struct{}{}
	}
	ok, admitted := f.hub.pwVerify("h", "right-pw")
	if ok || admitted {
		t.Fatalf("信号量占满时应 (false,false)，实际 (%v,%v)", ok, admitted)
	}
	if calls.Load() != 0 {
		t.Fatal("排队超时不得调用 bcrypt")
	}

	// 释放一个许可后恢复：这次真的跑了校验。
	<-f.hub.pwSem
	ok, admitted = f.hub.pwVerify("h", "right-pw")
	if !ok || !admitted {
		t.Fatalf("空出许可后应正常校验，实际 (%v,%v)", ok, admitted)
	}
	if calls.Load() != 1 {
		t.Fatalf("校验次数 = %d，want 1", calls.Load())
	}
	// 清理：把剩余许可取回，避免 TestCleanup 阶段挂住。
	for i := 0; i < channelPwConcurrency-1; i++ {
		<-f.hub.pwSem
	}
}

// TestChannelPwAdminBypassNotLimited 管理员旁路不消耗配额，也不受失败窗口影响。
func TestChannelPwAdminBypassNotLimited(t *testing.T) {
	f := newChatFixture(t)
	setVchPassword(t, f, "right-pw")
	vchID := strconv.FormatInt(f.vch.ID, 10)

	// 先把普通成员 bob 的配额打满。
	b := f.dial("2", "bob")
	for i := 0; i < channelPwFailLimit; i++ {
		b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "wrong"})
		wantError(t, b, ErrCodeBadPassword)
	}
	// owner（alice）不带密码仍放行。
	a := f.dial("1", "alice")
	mustJoin(t, a, vchID, "")
}

// TestChannelPwFailuresBounded 失败计数表有界：超过键上限时清掉过期键。
func TestChannelPwFailuresBounded(t *testing.T) {
	h := &Hub{pwFails: map[pwFailKey][]time.Time{}}
	old := time.Now().Add(-2 * channelPwFailWindow)
	for i := 0; i < pwFailKeyCap; i++ {
		h.pwFails[makePwFailKey(strconv.Itoa(i), "1")] = []time.Time{old}
	}
	h.pwRecordFailure(makePwFailKey("fresh", "1"), time.Now())
	if len(h.pwFails) != 1 { // 过期键全部清掉，只剩 fresh
		t.Fatalf("键数 = %d，want 1（过期键应被清理）", len(h.pwFails))
	}
}
