package authz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// TestCacheHitNoReload 未过期命中不重复打 Store。
func TestCacheHitNoReload(t *testing.T) {
	f := newFixture(t)
	w := &wrapperStore{Store: f.st}
	a := New(w, Options{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		yes(t, "Can", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
		if _, err := a.IsMember(ctx, f.member.ID, f.srv.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.countServer("ServerByID", f.srv.ID); n != 1 {
		t.Fatalf("缓存命中不得重复加载，实际加载 %d 次", n)
	}
	if n := w.count("ListRoles"); n != 1 {
		t.Fatalf("ListRoles 应只调一次，实际 %d", n)
	}
}

// TestTTLReload TTL 过期后重新加载。
func TestTTLReload(t *testing.T) {
	f := newFixture(t)
	w := &wrapperStore{Store: f.st}
	a := New(w, Options{TTL: 20 * time.Millisecond})
	ctx := context.Background()
	yes(t, "第一次", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	yes(t, "期内", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	if n := w.countServer("ServerByID", f.srv.ID); n != 1 {
		t.Fatalf("期内不该重载，实际 %d", n)
	}
	time.Sleep(40 * time.Millisecond)
	yes(t, "过期后", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	if n := w.countServer("ServerByID", f.srv.ID); n != 2 {
		t.Fatalf("过期后应重载一次，实际 %d", n)
	}
}

// TestInvalidate 两个失效接口都立即生效。
func TestInvalidate(t *testing.T) {
	f := newFixture(t)
	w := &wrapperStore{Store: f.st}
	a := New(w, Options{})
	ctx := context.Background()
	yes(t, "第一次", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	a.InvalidateServer(f.srv.ID)
	yes(t, "InvalidateServer 后", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	if n := w.countServer("ServerByID", f.srv.ID); n != 2 {
		t.Fatalf("InvalidateServer 后应立即重载，实际 %d", n)
	}
	a.InvalidateAll()
	yes(t, "InvalidateAll 后", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	if n := w.countServer("ServerByID", f.srv.ID); n != 3 {
		t.Fatalf("InvalidateAll 后应立即重载，实际 %d", n)
	}
}

// TestMaxServersEviction LRU 容量上限：超量淘汰最久未用，不允许无界增长。
func TestMaxServersEviction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 再建两服（自动带 builtin 角色）。
	srvs := []int64{f.srv.ID}
	for i := 0; i < 2; i++ {
		s := &store.Server{Name: "evict", OwnerID: f.owner.ID, IconName: "music", IconColor: "#4FA4F3"}
		if err := f.st.CreateServer(ctx, s); err != nil {
			t.Fatal(err)
		}
		srvs = append(srvs, s.ID)
	}
	w := &wrapperStore{Store: f.st}
	a := New(w, Options{MaxServers: 2})
	for _, id := range srvs {
		if _, err := a.IsMember(ctx, f.member.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	// 三服全载后 A 应已被淘汰。
	if a.lru.Len() > 2 {
		t.Fatalf("缓存条目数超上限: %d", a.lru.Len())
	}
	if _, err := a.IsMember(ctx, f.member.ID, srvs[0]); err != nil {
		t.Fatal(err)
	}
	if n := w.countServer("ServerByID", srvs[0]); n != 2 {
		t.Fatalf("被淘汰的服应重新加载，实际 %d", n)
	}
	// C 最近刚用过（插在 B 之后），B 是最久未用：再载 B 验证。
	if _, err := a.IsMember(ctx, f.member.ID, srvs[1]); err != nil {
		t.Fatal(err)
	}
	if n := w.countServer("ServerByID", srvs[1]); n != 2 {
		t.Fatalf("第二次淘汰对象应重新加载，实际 %d", n)
	}
}

// TestSingleflightOneLoad 并发同服冷启动只打一次 Store。
func TestSingleflightOneLoad(t *testing.T) {
	f := newFixture(t)
	// startedList 缓冲开满：万一单飞失效，测试要干净失败而不是发送端挂死。
	w := &wrapperStore{Store: f.st, blockList: make(chan struct{}), startedList: make(chan struct{}, 64)}
	a := New(w, Options{})
	ctx := context.Background()
	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Can(ctx, f.member.ID, f.ch.ID, store.Speak)
			errs <- err
		}()
	}
	// 第一个进入加载后放行；其余并发者应挂在单飞上而不是各自加载。
	<-w.startedList
	close(w.blockList)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Can: %v", err)
		}
	}
	if got := w.countServer("ServerByID", f.srv.ID); got != 1 {
		t.Fatalf("并发同服应只加载一次，实际 %d", got)
	}
	if got := w.count("ListRoles"); got != 1 {
		t.Fatalf("ListRoles 应只调一次，实际 %d", got)
	}
}

// TestLoadFailureFailClosed 加载失败：返回 error 而不是陈旧快照，旧条目删除；
// 恢复后重新加载。
func TestLoadFailureFailClosed(t *testing.T) {
	f := newFixture(t)
	w := &wrapperStore{Store: f.st}
	a := New(w, Options{TTL: 20 * time.Millisecond})
	ctx := context.Background()
	// v1 成功（此时成员可 Speak）。
	yes(t, "第一次加载", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })

	boom := errors.New("db down")
	w.listRolesFn = func(context.Context, int64) ([]store.Role, error) { return nil, boom }
	time.Sleep(40 * time.Millisecond) // 过期 → 重载
	ok, err := a.Can(ctx, f.member.ID, f.ch.ID, store.Speak)
	if ok || !errors.Is(err, boom) {
		t.Fatalf("重载失败必须返回 error，不允许回落旧快照，实际 (%v, %v)", ok, err)
	}
	// 旧条目已删：即便故障持续，每次调用都是新加载（fail-closed）而不是缓存兜底。
	_, _ = a.Can(ctx, f.member.ID, f.ch.ID, store.Speak)
	if n := w.countServer("ServerByID", f.srv.ID); n != 3 {
		t.Fatalf("失败后不得留缓存，实际加载 %d 次（应 3：成功/失败/再失败）", n)
	}
	// 恢复后正常加载。
	w.listRolesFn = nil
	yes(t, "恢复后", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	if n := w.countServer("ServerByID", f.srv.ID); n != 4 {
		t.Fatalf("恢复后应再加载，实际 %d", n)
	}
}

// TestStaleServedDuringReload 重载在飞时旧快照继续服务（已注释钉住的策略）：
// 权限变更在重载完成后才生效，避免抖动；失败路径见 TestLoadFailureFailClosed。
func TestStaleServedDuringReload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := &wrapperStore{Store: f.st}
	a := New(w, Options{TTL: 20 * time.Millisecond})

	// v1：成员可 ViewChannel（此时不设 blockList，首载要能完成）。
	yes(t, "v1", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })
	time.Sleep(40 * time.Millisecond) // 过期

	// 之后的重载卡在 ListRoles。
	w.startedList = make(chan struct{}, 4)
	w.blockList = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel)
	}()
	<-w.startedList

	// v2 落库（@everyone deny ViewChannel）——重载还没读到覆盖表也没事，
	// 这里验证的是「加载期间用旧快照」。
	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetRole, TargetID: f.everyoneID, Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	yes(t, "重载期间旧快照继续服务", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })

	// 放行重载，等它提交 v2 后新策略生效。
	close(w.blockList)
	<-done
	no(t, "重载完成后 v2 生效", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })
}

// TestConcurrentCanInvalidate Can 与 Invalidate 并发（-race 验证并发安全）。
func TestConcurrentCanInvalidate(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{TTL: 5 * time.Millisecond})
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ok, err := a.Can(ctx, f.member.ID, f.ch.ID, store.Speak)
				if err != nil {
					t.Errorf("Can: %v", err)
					return
				}
				if !ok {
					t.Errorf("成员应有 Speak")
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a.InvalidateServer(f.srv.ID)
				a.InvalidateAll()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	// 收尾仍可用。
	yes(t, "收尾", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
}
