package authz

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// cacheEntry LRU 节点：快照 + 进入时间。
type cacheEntry struct {
	serverID int64
	snap     *serverPolicy
	loadedAt time.Time
}

// loadCall 同服加载的 singleflight 句柄：并发请求等同一个 done。
type loadCall struct {
	done chan struct{}
	snap *serverPolicy
	err  error
	// epoch 发起加载时的失效代数；提交时代数已变则丢弃结果（Invalidate 竞态防护）。
	epoch uint64
}

type Authorizer struct {
	st  store.Store
	ttl time.Duration
	max int

	mu      sync.Mutex
	lru     *list.List              // front = 最近使用
	entries map[int64]*list.Element // serverID → *cacheEntry
	loading map[int64]*loadCall
	// epoch 全局失效代数：InvalidateServer/InvalidateAll 都会 +1，让在途加载的
	// 结果作废（宁可多加载一次，也不能让失效前的旧策略在失效后入库）。
	epoch uint64
}

// 策略缓存默认值。
const (
	DefaultTTL        = 10 * time.Second
	DefaultMaxServers = 256
)

// Options 缓存参数；零值/负值回落默认。
type Options struct {
	TTL        time.Duration
	MaxServers int
}

// New 创建 Authorizer。Store 访问只发生在快照加载（单飞），判断热路径纯内存。
func New(st store.Store, opts Options) *Authorizer {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	max := opts.MaxServers
	if max <= 0 {
		max = DefaultMaxServers
	}
	return &Authorizer{
		st:      st,
		ttl:     ttl,
		max:     max,
		lru:     list.New(),
		entries: map[int64]*list.Element{},
		loading: map[int64]*loadCall{},
	}
}

// policyFor 取服务器快照：
//   - 命中未过期：直接返回，不打 Store；
//   - 过期且已有加载在飞：旧快照继续服务（避免重载抖动）；
//   - 过期且无加载：本调用触发重载并等结果（失败即 error，不回落旧快照）；
//   - 缓存缺失：等在飞加载或发起加载，失败即 error（fail-closed）。
//
// 加载失败会删除旧条目：权限变更后的旧策略绝不继续生效，宁可暂时 500。
func (a *Authorizer) policyFor(ctx context.Context, serverID int64) (*serverPolicy, error) {
	a.mu.Lock()
	if el := a.entries[serverID]; el != nil {
		e := el.Value.(*cacheEntry)
		if time.Since(e.loadedAt) < a.ttl {
			a.lru.MoveToFront(el)
			snap := e.snap
			a.mu.Unlock()
			return snap, nil
		}
		if call := a.loading[serverID]; call != nil {
			stale := e.snap
			a.mu.Unlock()
			return stale, nil
		}
		call := a.beginLoadLocked(serverID)
		a.mu.Unlock()
		return a.doLoad(ctx, serverID, call)
	}
	if call := a.loading[serverID]; call != nil {
		a.mu.Unlock()
		return a.waitLoad(ctx, call)
	}
	call := a.beginLoadLocked(serverID)
	a.mu.Unlock()
	return a.doLoad(ctx, serverID, call)
}

// beginLoadLocked 登记单飞（调用方持 a.mu）。
func (a *Authorizer) beginLoadLocked(serverID int64) *loadCall {
	call := &loadCall{done: make(chan struct{}), epoch: a.epoch}
	a.loading[serverID] = call
	return call
}

// doLoad 发起方执行真正的 Store 加载并提交快照。
func (a *Authorizer) doLoad(ctx context.Context, serverID int64, call *loadCall) (*serverPolicy, error) {
	snap, err := a.loadFromStore(ctx, serverID)
	a.mu.Lock()
	if err != nil {
		// fail-closed：删旧条目，权限变更后的旧策略不再继续服务。
		a.removeLocked(serverID)
		call.err = err
		delete(a.loading, serverID)
		close(call.done)
		a.mu.Unlock()
		return nil, err
	}
	if call.epoch == a.epoch {
		a.insertLocked(serverID, snap)
	}
	call.snap = snap
	delete(a.loading, serverID)
	close(call.done)
	a.mu.Unlock()
	return snap, nil
}

// waitLoad 等待同服在飞加载（缓存缺失路径；过期路径有旧快照可服务，不进这里）。
func (a *Authorizer) waitLoad(ctx context.Context, call *loadCall) (*serverPolicy, error) {
	select {
	case <-call.done:
		return call.snap, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// insertLocked 入缓存并按 LRU 淘汰（调用方持 a.mu）。
func (a *Authorizer) insertLocked(serverID int64, snap *serverPolicy) {
	if el := a.entries[serverID]; el != nil {
		el.Value.(*cacheEntry).snap = snap
		el.Value.(*cacheEntry).loadedAt = time.Now()
		a.lru.MoveToFront(el)
		return
	}
	e := &cacheEntry{serverID: serverID, snap: snap, loadedAt: time.Now()}
	a.entries[serverID] = a.lru.PushFront(e)
	for a.lru.Len() > a.max {
		a.evictOldestLocked()
	}
}

// removeLocked 删单个条目（调用方持 a.mu）。
func (a *Authorizer) removeLocked(serverID int64) {
	if el := a.entries[serverID]; el != nil {
		a.lru.Remove(el)
		delete(a.entries, serverID)
	}
}

// evictOldestLocked LRU 淘汰最久未用（调用方持 a.mu）。
func (a *Authorizer) evictOldestLocked() {
	back := a.lru.Back()
	if back == nil {
		return
	}
	e := back.Value.(*cacheEntry)
	a.lru.Remove(back)
	delete(a.entries, e.serverID)
}

// InvalidateServer 立即作废某服快照；在途加载的结果不再入库。
func (a *Authorizer) InvalidateServer(serverID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removeLocked(serverID)
	a.epoch++
}

// InvalidateAll 清空全部快照；所有在途加载的结果不再入库。
func (a *Authorizer) InvalidateAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lru.Init()
	a.entries = map[int64]*list.Element{}
	a.epoch++
}

// loadFromStore 一次性拉齐服务器权限数据。ListRoles/RoleAssignmentsOfServer 的
// ErrNotFound 按空处理（legacy 旧库没有 0010 表数据，走静态模板回退，不许报错）；
// 其余错误一律上抛，不能把故障伪装成无权限。
func (a *Authorizer) loadFromStore(ctx context.Context, serverID int64) (*serverPolicy, error) {
	srv, err := a.st.ServerByID(ctx, serverID)
	if err != nil {
		return nil, err
	}
	roles, err := a.st.ListRoles(ctx, serverID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	assigns, err := a.st.RoleAssignmentsOfServer(ctx, serverID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	members, err := a.st.MembersOfServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	ows, err := a.st.ChannelOverwritesOfServer(ctx, serverID)
	if err != nil {
		return nil, err
	}

	snap := &serverPolicy{
		ownerID:     srv.OwnerID,
		roles:       make(map[int64]store.Role, len(roles)),
		builtin:     map[string]int64{},
		memberRoles: map[int64][]int64{},
		members:     make(map[int64]store.MemberRole, len(members)),
		overwrites:  map[int64][]store.ChannelOverwrite{},
		loadedAt:    time.Now(),
	}
	for _, r := range roles {
		snap.roles[r.ID] = r
		if r.IsBuiltin() {
			snap.builtin[r.BuiltinKey] = r.ID
		}
	}
	for _, as := range assigns {
		snap.memberRoles[as.UserID] = append(snap.memberRoles[as.UserID], as.RoleID)
	}
	for _, m := range members {
		snap.members[m.UserID] = m.Role
	}
	for _, o := range ows {
		snap.overwrites[o.ChannelID] = append(snap.overwrites[o.ChannelID], o)
	}
	return snap, nil
}
