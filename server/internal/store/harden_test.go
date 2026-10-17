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
	// 邀请校验前置：须先建服务器。
	if err := m.CreateServer(ctx, &Server{Name: "s", OwnerID: 1}); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	inv := &Invite{Code: "race-invite", ServerID: 1, CreatorID: 1, MaxUses: 5}
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
	if err := m.CreateServer(ctx, &Server{Name: "s", OwnerID: 1}); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	inv := &Invite{Code: "expired", ServerID: 1, CreatorID: 1, MaxUses: 0, ExpiresAt: &past}
	if err := m.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := m.UseInvite(ctx, "expired"); err != ErrExpired {
		t.Fatalf("过期邀请应返回 ErrExpired，实际 %v", err)
	}
}
