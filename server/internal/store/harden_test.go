package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestMemIncrCapRace 并发竞态：20 个请求、limit=5，成功数恰 5（#22）。
func TestMemIncrCapRace(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	const limit = 5
	const total = 20

	var wg sync.WaitGroup
	results := make([]int64, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			n, err := m.IncrCap(ctx, "race", time.Minute, limit)
			if err != nil {
				t.Errorf("IncrCap: %v", err)
				return
			}
			results[idx] = n
		}(i)
	}
	wg.Wait()

	success := 0
	for _, n := range results {
		if n <= limit {
			success++
		} else if n != limit+1 {
			t.Fatalf("超限值应为 %d，实际 %d", limit+1, n)
		}
	}
	if success != limit {
		t.Fatalf("成功数应恰为 %d，实际 %d", limit, success)
	}
}

// TestMemIncrCapSingle IncrCap 基本语义：从 1 开始，到 cap 后返回 cap+1 不增长。
func TestMemIncrCapSingle(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	n, err := m.IncrCap(ctx, "k", time.Minute, 3)
	if n != 1 || err != nil {
		t.Fatalf("首次 = %d %v", n, err)
	}
	n, _ = m.IncrCap(ctx, "k", time.Minute, 3)
	if n != 2 {
		t.Fatalf("第二次 = %d", n)
	}
	n, _ = m.IncrCap(ctx, "k", time.Minute, 3)
	if n != 3 {
		t.Fatalf("第三次 = %d", n)
	}
	// 已达 cap：不增长，返回 cap+1。
	n, _ = m.IncrCap(ctx, "k", time.Minute, 3)
	if n != 4 {
		t.Fatalf("超限 = %d，应为 4", n)
	}
	// 值仍在 3，未被写为 4。
	cur, _ := m.Get(ctx, "k")
	if cur != "3" {
		t.Fatalf("值应保持 3，实际 %q", cur)
	}
}

// TestMemUseInviteRace UseInvite 竞态：max_uses=5、20 并发，成功恰 5（#22）。
func TestMemUseInviteRace(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	// 邀请校验前置：须先建服务器（ID 由 Snowflake 赋值，不能写死 1）。
	sv := &Server{Name: "s", OwnerID: 1}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	inv := &Invite{Code: "race-invite", ServerID: sv.ID, CreatorID: 1, MaxUses: 5}
	if err := m.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = m.UseInvite(ctx, "race-invite")
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 5 {
		t.Fatalf("成功数应恰为 5，实际 %d", ok)
	}
}

// TestMemUseInviteExpired 过期邀请在原子路径被拒（#22）。
func TestMemUseInviteExpired(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	sv := &Server{Name: "s", OwnerID: 1}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	inv := &Invite{Code: "expired", ServerID: sv.ID, CreatorID: 1, MaxUses: 0, ExpiresAt: &past}
	if err := m.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := m.UseInvite(ctx, "expired"); err != ErrExpired {
		t.Fatalf("过期邀请应返回 ErrExpired，实际 %v", err)
	}
}

// TestMemUseInviteAndAddMemberRace 邀请码消费与加入成员同一临界区（#22）：
// 20 个不同用户并发抢 max_uses=5 的码，成功恰 5 人，成员数与 uses 都为 5。
func TestMemUseInviteAndAddMemberRace(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	sv := &Server{Name: "s", OwnerID: 1}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if err := m.CreateInvite(ctx, &Invite{Code: "join-race", ServerID: sv.ID, CreatorID: 1, MaxUses: 5}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			mem, err := m.UseInviteAndAddMember(ctx, "join-race", uid, RoleMember)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				if mem == nil || mem.ServerID != sv.ID || mem.Role != RoleMember {
					t.Errorf("返回成员不符: %+v", mem)
				}
				ok++
			case err != ErrConflict:
				t.Errorf("失败原因应为 ErrConflict，实际 %v", err)
			}
		}(int64(i + 1))
	}
	wg.Wait()
	if ok != 5 {
		t.Fatalf("成功数应恰为 5，实际 %d", ok)
	}
	members, err := m.MembersOfServer(ctx, sv.ID)
	if err != nil || len(members) != 5 {
		t.Fatalf("成员数应恰为 5: %d %v", len(members), err)
	}
	inv, err := m.InviteByCode(ctx, "join-race")
	if err != nil || inv.Uses != 5 {
		t.Fatalf("uses 应恰为 5: %+v %v", inv, err)
	}
}

// TestMemUseInviteAndAddMemberConflictDoesNotConsume 同一用户重复加入返回
// ErrConflict，且不消耗邀请码次数（#22）。
func TestMemUseInviteAndAddMemberConflictDoesNotConsume(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	sv := &Server{Name: "s", OwnerID: 1}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if err := m.CreateInvite(ctx, &Invite{Code: "join-once", ServerID: sv.ID, CreatorID: 1, MaxUses: 2}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, err := m.UseInviteAndAddMember(ctx, "join-once", 7, RoleMember); err != nil {
		t.Fatalf("首次加入: %v", err)
	}
	if _, err := m.UseInviteAndAddMember(ctx, "join-once", 7, RoleMember); err != ErrConflict {
		t.Fatalf("重复加入应 ErrConflict，实际 %v", err)
	}
	inv, _ := m.InviteByCode(ctx, "join-once")
	if inv.Uses != 1 {
		t.Fatalf("成员冲突不应消耗邀请码，uses = %d", inv.Uses)
	}
	if _, err := m.UseInviteAndAddMember(ctx, "join-once", 8, RoleMember); err != nil {
		t.Fatalf("第二个用户加入: %v", err)
	}
	if _, err := m.UseInviteAndAddMember(ctx, "join-once", 9, RoleMember); err != ErrConflict {
		t.Fatalf("次数用尽应 ErrConflict，实际 %v", err)
	}
	if _, err := m.Member(ctx, sv.ID, 9); err != ErrNotFound {
		t.Fatalf("次数用尽后不应加入: %v", err)
	}
}
