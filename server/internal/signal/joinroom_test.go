package signal

import (
	"context"
	"strconv"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #37 进房门：密码（错误码 forbidden/bad_password/管理员旁路）与人数上限
// （quota_exceeded、满员不踢原房、自己顶替）。

// setVchPassword 给语音房设密码。必须先读当前行再写：UpdateChannel 是全行写，
// 拿旧快照写回会把其它字段（user_limit 等）一起抹掉。
func setVchPassword(t *testing.T, f *chatFixture, plain string) {
	t.Helper()
	hash, err := auth.HashPassword(plain)
	if err != nil {
		t.Fatal(err)
	}
	ch := loadVch(t, f)
	ch.PasswordHash = hash
	if err := f.st.UpdateChannel(context.Background(), ch); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
}

// setVchLimit 设置语音房人数上限（0 = 不限）。同样先读后写（全行写语义）。
func setVchLimit(t *testing.T, f *chatFixture, limit int) {
	t.Helper()
	ch := loadVch(t, f)
	ch.UserLimit = limit
	if err := f.st.UpdateChannel(context.Background(), ch); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
}

// loadVch 从 store 读语音房当前行（f.vch 只是创建时的快照，不可直接写回）。
func loadVch(t *testing.T, f *chatFixture) *store.Channel {
	t.Helper()
	ch, err := f.st.ChannelByID(context.Background(), f.vch.ID)
	if err != nil {
		t.Fatalf("ChannelByID: %v", err)
	}
	return ch
}

// mustJoin 指定客户端进房并等到 joined。
func mustJoin(t *testing.T, c *wsClient, chID string, password string) {
	t.Helper()
	c.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID, Password: password})
	c.recvUntil(TypeChannelJoined)
}

// TestJoinPasswordGate 密码门（#37）：无密码频道放行；有密码不带 → forbidden；
// 带错 → bad_password；带对 → joined；已在房 → already_in_channel（先于密码门）。
func TestJoinPasswordGate(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	// 无密码：不带/带任意密码都放行（带了忽略）。
	a := f.dial("1", "alice")
	mustJoin(t, a, vchID, "")
	b := f.dial("2", "bob")
	mustJoin(t, b, vchID, "随便带的")

	// 有密码：bob 重进（先离房）——不带 → forbidden；带错 → bad_password；带对 → joined。
	b.send(TypeChannelLeave, ChannelLeaveData{})
	setVchPassword(t, f, "pw-join")
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, b, ErrCodeForbidden)
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "wrong"})
	wantError(t, b, ErrCodeBadPassword)
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "pw-join"})
	b.recvUntil(TypeChannelJoined)

	// 已在房（不带密码）→ already_in_channel：密码门之前判，重发 join 不被拦。
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, b, ErrCodeAlreadyInChan)
}

// TestJoinPasswordAdminBypass 管理员（owner/Administrator）旁路密码；人数上限不旁路。
func TestJoinPasswordAdminBypass(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	setVchPassword(t, f, "pw-only")
	setVchLimit(t, f, 2)

	// owner 不带密码放行（密码旁路）。
	a := f.dial("1", "alice")
	mustJoin(t, a, vchID, "")
	// 普通成员不带密码仍拒。
	b := f.dial("2", "bob")
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, b, ErrCodeForbidden)

	// bob 带密码进（2/2 满员）。
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "pw-only"})
	b.recvUntil(TypeChannelJoined)

	// owner 离房腾位，mallory 带密码补满（bob+m，2/2）。
	a.send(TypeChannelLeave, ChannelLeaveData{})
	m := f.dial("9", "mallory")
	m.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID, Password: "pw-only"})
	m.recvUntil(TypeChannelJoined)

	// 人数上限不旁路：owner 本人（不在房）回来也被满员拦——密码可绕、容量不可绕。
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, a, ErrCodeQuotaExceeded)
	if n := f.hub.roomMemberCount(vchID); n != 2 {
		t.Fatalf("被拒后原房人数 = %d，want 2", n)
	}
}

// TestJoinUserLimit 人数上限（#37）：user_limit 取小；满员被拒不得踢原房；
// 自己另一设备在目标房可顶替。
func TestJoinUserLimit(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	setVchLimit(t, f, 1)

	a := f.dial("1", "alice")
	mustJoin(t, a, vchID, "")

	// 第二人（普通成员）满员拒。
	b := f.dial("2", "bob")
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, b, ErrCodeQuotaExceeded)

	// 原房不受影响：仍是 1 人，a 照常收事件。
	if n := f.hub.roomMemberCount(vchID); n != 1 {
		t.Fatalf("满员被拒后原房人数 = %d，want 1", n)
	}
	// a 还在房（roomID 未被动过）。
	if got := f.hub.roomMember(vchID, "1"); got == nil {
		t.Fatal("满员被拒后原房成员应保留")
	}

	// 自己另一设备顶替：同 uid 新连接进满员房放行，旧连接被 voice_taken。
	a2 := f.dial("1:web:sa2", "alice")
	a2.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	a2.recvUntil(TypeChannelJoined)
	if n := f.hub.roomMemberCount(vchID); n != 1 {
		t.Fatalf("顶替后房内人数 = %d，want 1", n)
	}
	if got := f.hub.roomMember(vchID, "1"); got == nil {
		t.Fatal("顶替后应为新连接")
	}
}

// TestJoinUserLimitZero 不限人数（0）回落 hub 全局上限语义：0 不额外设限。
func TestJoinUserLimitZero(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	// 显式 0 = 不限（与缺省同）。
	setVchLimit(t, f, 0)
	a := f.dial("1", "alice")
	mustJoin(t, a, vchID, "")
	b := f.dial("2", "bob")
	mustJoin(t, b, vchID, "")
	if n := f.hub.roomMemberCount(vchID); n != 2 {
		t.Fatalf("0 上限应不限人数，房内 %d", n)
	}
}
