package signal

// ws_broadcast_shortcircuit_test.go P1-7 P3：令牌连接计数 tokenConns 与广播短路。
// 计数只是性能短路，不决定正确性。这里钉住三件事：计数与 conns 实数严格同步；
// 计数为 0 时 JWT 路径零额外查询；计数失真时目标循环的逐连接判定仍挡住不可见连接。

import (
	"math/rand"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// fakeConn 只带路由字段的假连接：kind 为空 = 无主体（dev/测试直连），其余按令牌
// 种类挂 Principal。register/unregister 只依赖 userID、dev 与 principal。
func fakeConn(uid, dev, kind string) *client {
	c := &client{userID: uid, dev: dev}
	if kind != "" {
		c.principal = &token.Principal{Claims: &auth.Claims{}, Kind: kind}
	}
	return c
}

// tokenConnsDrift 持 h.mu 同时读计数与 conns 里令牌连接的实数。计数的增减都在
// h.mu 内完成，持锁读到的两个数必然自洽；不一致即漂移。
func tokenConnsDrift(h *Hub) (counter, actual int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		if isTokenConn(c) {
			actual++
		}
	}
	return h.tokenConns.Load(), actual
}

// wantTokenConns 断言计数 == 实数 == want。只能在测试 goroutine 里调用（Fatalf）。
func wantTokenConns(t *testing.T, h *Hub, step string, want int64) {
	t.Helper()
	got, actual := tokenConnsDrift(h)
	if got != actual || got != want {
		t.Fatalf("%s：tokenConns=%d conns 实数=%d，want %d", step, got, actual, want)
	}
}

// waitTokenConns 轮询等待计数与实数都达到 want（旧连接的 cleanup 是异步的）。
func waitTokenConns(t *testing.T, h *Hub, step string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, actual := tokenConnsDrift(h)
		if got == want && actual == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s：tokenConns=%d conns 实数=%d，want %d", step, got, actual, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTokenConnsCounterRegisterUnregister 注册/注销：只有 pat/bot 计数，JWT 与无
// 主体连接不计；重复注销同一连接不重复减。
func TestTokenConnsCounterRegisterUnregister(t *testing.T) {
	h := &Hub{conns: make(map[connKey]*client)}
	wantTokenConns(t, h, "初始", 0)

	pat := fakeConn("2", "api", token.KindPAT)
	bot := fakeConn("3", "api", token.KindBot)
	jwt := fakeConn("4", "desktop", token.KindJWT)
	plain := fakeConn("5", "desktop", "")
	for _, c := range []*client{pat, bot, jwt, plain} {
		h.register(c)
	}
	wantTokenConns(t, h, "注册 pat/bot/jwt/无主体", 2)

	h.unregister(jwt)
	h.unregister(plain)
	wantTokenConns(t, h, "注销 JWT 与无主体连接不动计数", 2)
	h.unregister(pat)
	wantTokenConns(t, h, "注销 pat", 1)
	h.unregister(pat)
	wantTokenConns(t, h, "重复注销同一连接不重复减", 1)
	h.unregister(bot)
	wantTokenConns(t, h, "注销 bot", 0)
}

// TestTokenConnsCounterReplace 同 uid+dev 顶替：令牌顶 JWT、JWT 顶令牌、令牌顶令牌
// 等组合下计数都与实数一致；被顶替的旧连接迟到的 unregister 不得再动计数。
func TestTokenConnsCounterReplace(t *testing.T) {
	cases := []struct {
		name         string
		oldKind      string
		newKind      string
		afterReplace int64
	}{
		{"令牌顶 JWT", token.KindJWT, token.KindPAT, 1},
		{"JWT 顶令牌", token.KindPAT, token.KindJWT, 0},
		{"令牌顶令牌", token.KindPAT, token.KindBot, 1},
		{"令牌顶无主体", "", token.KindBot, 1},
		{"无主体顶令牌", token.KindBot, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hub{conns: make(map[connKey]*client)}
			old := fakeConn("2", "api", tc.oldKind)
			cur := fakeConn("2", "api", tc.newKind)
			h.register(old)
			if got, _ := h.register(cur); got != old {
				t.Fatalf("register 应返回被顶替的旧连接，got %p want %p", got, old)
			}
			wantTokenConns(t, h, "顶替后", tc.afterReplace)
			// 旧连接随后 cleanup：unregister 核对 conns[key]==c，已被顶替则不动计数。
			h.unregister(old)
			wantTokenConns(t, h, "旧连接迟到注销", tc.afterReplace)
			h.unregister(cur)
			wantTokenConns(t, h, "新连接注销", 0)
		})
	}
}

// TestTokenConnsCounterConcurrent 多 goroutine 随机注册/顶替/注销，同时有校验协程
// 持锁比对计数与实数。-race 下同时验证读侧无锁 Load 与锁内增减不冲突。
func TestTokenConnsCounterConcurrent(t *testing.T) {
	h := &Hub{conns: make(map[connKey]*client)}
	kinds := []string{token.KindPAT, token.KindBot, token.KindJWT, ""}
	devs := []string{"api", "desktop"}
	const workers, rounds = 8, 400

	stop := make(chan struct{})
	ready := make(chan struct{})
	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		first := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			if got, actual := tokenConnsDrift(h); got != actual {
				t.Errorf("并发中计数漂移：tokenConns=%d conns 实数=%d", got, actual)
				if first {
					close(ready)
				}
				return
			}
			if first {
				first = false
				close(ready) // 校验协程确实在跑之后才放 worker 开工
			}
			runtime.Gosched()
		}
	}()
	<-ready

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []*client
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			var mine []*client
			for i := 0; i < rounds; i++ {
				if len(mine) > 0 && rng.Intn(3) == 0 {
					h.unregister(mine[rng.Intn(len(mine))])
					continue
				}
				// 3 个 uid × 2 个设备，故意撞槽位制造顶替。
				c := fakeConn(strconv.Itoa(rng.Intn(3)+1), devs[rng.Intn(len(devs))], kinds[rng.Intn(len(kinds))])
				h.register(c)
				mine = append(mine, c)
			}
			mu.Lock()
			all = append(all, mine...)
			mu.Unlock()
		}(int64(w) + 1)
	}
	wg.Wait()
	close(stop)
	<-checkDone

	if got, actual := tokenConnsDrift(h); got != actual {
		t.Fatalf("并发结束后计数漂移：tokenConns=%d conns 实数=%d", got, actual)
	}
	// 收尾：已被顶替的连接 unregister 是 no-op，仍在槽位上的被摘除。
	for _, c := range all {
		h.unregister(c)
	}
	wantTokenConns(t, h, "全部注销后", 0)
	h.mu.Lock()
	left := len(h.conns)
	h.mu.Unlock()
	if left != 0 {
		t.Fatalf("全部注销后 conns 仍有 %d 条", left)
	}
}

// TestTokenConnsCounterViaWS 经真实 ServeWS 走一遍三种顶替：令牌顶令牌、JWT 顶令牌、
// 令牌顶 JWT（同 uid+dev=api，sid 不同走 kicked 路径），最后关闭。计数全程与实数一致，
// 旧连接异步 cleanup 不得造成漂移。
func TestTokenConnsCounterViaWS(t *testing.T) {
	f := newScopeHub(t)
	tokA, _ := f.issueUserToken(t, 2, token.KindPAT, []string{token.ScopeMessagesRead})
	tokB, _ := f.issueUserToken(t, 2, token.KindPAT, []string{token.ScopeMessagesRead})

	a := f.dialBot(t, tokA)
	waitTokenConns(t, f.hub, "令牌 A 建连", 1)

	b := f.dialBot(t, tokB) // 令牌顶令牌
	a.readCloseCode()
	waitTokenConns(t, f.hub, "令牌 B 顶替令牌 A", 1)

	j := f.dial("2:api:sjwt", "bob-jwt") // JWT 顶令牌
	b.readCloseCode()
	waitTokenConns(t, f.hub, "JWT 顶替令牌 B", 0)

	c := f.dialBot(t, tokA) // 令牌顶 JWT
	j.readCloseCode()
	waitTokenConns(t, f.hub, "令牌 A 顶替 JWT", 1)

	_ = c.conn.CloseNow()
	waitTokenConns(t, f.hub, "最后一条令牌连接关闭", 0)
	waitConnsEmpty(t, f.hub)

	// 稳定窗口：迟到的 cleanup 若重复减，计数会在这段时间内偏离实数。
	end := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(end) {
		wantTokenConns(t, f.hub, "稳定窗口", 0)
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWSBroadcastShortCircuitNoTokenConns 无令牌连接：JWT 连接照常收到带频道事件，
// 授权服务零可见性查询；令牌连接上线后恢复判定（同一次广播内同一连接只查一次）；
// 令牌连接断开、计数回 0 后重新短路。
func TestWSBroadcastShortCircuitNoTokenConns(t *testing.T) {
	f := newScopeHub(t)
	az := &stubAuthz{}
	f.hub.authz = az
	chID := strconv.FormatInt(f.ch.ID, 10)
	plain, _ := f.issueUserToken(t, 2, token.KindPAT, []string{token.ScopeMessagesRead})
	jwtB := newPump(t, f.dial("2", "bob"))

	edited := func(content string) {
		t.Helper()
		f.hub.BroadcastMessageEdited([]string{"2"}, chID, "1", content, time.Now().UnixMilli(), nil)
	}
	wantEdited := func(p *pump, content string) {
		t.Helper()
		var d MessageEditedData
		if err := p.until(TypeMessageEdited).Decode(&d); err != nil || d.Content != content {
			t.Fatalf("message.edited = %+v err=%v，want content=%q", d, err, content)
		}
	}
	authzCalls := func() int {
		az.mu.Lock()
		defer az.mu.Unlock()
		return az.calls[2]
	}

	if n := f.hub.tokenConns.Load(); n != 0 {
		t.Fatalf("起始 tokenConns = %d，want 0", n)
	}
	edited("p1")
	wantEdited(jwtB, "p1")
	if n := authzCalls(); n != 0 {
		t.Fatalf("无令牌连接时不应有可见性查询，实际 %d 次", n)
	}

	tokC := f.dialBot(t, plain)
	tok := newPump(t, tokC)
	edited("p2")
	wantEdited(jwtB, "p2")
	wantEdited(tok, "p2") // 桩全放行
	if n := authzCalls(); n != 1 {
		t.Fatalf("有令牌连接时同一次广播应只查 1 次，实际 %d 次", n)
	}

	_ = tokC.conn.CloseNow()
	waitTokenConns(t, f.hub, "令牌连接断开", 0)
	edited("p3")
	wantEdited(jwtB, "p3")
	if n := authzCalls(); n != 1 {
		t.Fatalf("计数回 0 后应重新短路，查询次数应仍为 1，实际 %d", n)
	}
}

// TestWSBroadcastFallbackIndependentOfCounter 计数失真（模拟「计数读到 0 之后令牌连接
// 才注册」）时，目标循环的逐连接判定仍把不可见连接挡在实时投递之外：正确性不依赖计数。
// 该窗口里环可能已写入完整载荷，续传复核必须扣留它。
func TestWSBroadcastFallbackIndependentOfCounter(t *testing.T) {
	f := newScopeHub(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	plain, tid := f.issueUserToken(t, 2, token.KindPAT, []string{token.ScopeGuildManage})
	tok := newPump(t, f.dialBot(t, plain))
	jwtB := newPump(t, f.dial("2", "bob"))

	f.hub.mu.Lock()
	f.hub.tokenConns.Store(0)
	f.hub.mu.Unlock()
	defer func() {
		// 还原到真实值，之后连接 cleanup 的 Add(-1) 才不会把计数减成负数。
		f.hub.mu.Lock()
		f.hub.tokenConns.Store(1)
		f.hub.mu.Unlock()
	}()

	f.hub.BroadcastMessageEdited([]string{"2"}, chID, "1", "fallback", time.Now().UnixMilli(), nil)
	jwtB.until(TypeMessageEdited)
	expectNoTypes(t, tok, 300*time.Millisecond, TypeMessageEdited)

	// 环里无论是否留有这条事件，续传复核都不得把它补发给该令牌连接。
	events, _ := f.hub.evlog.Replay(tokenTestSid(tid), 1)
	c := f.hub.connByDevice("2", "api")
	if c == nil {
		t.Fatal("令牌连接不在 conns 里")
	}
	if out, _ := f.hub.filterReplayByPerm(c, events); len(out) != 0 {
		t.Fatalf("续传复核应扣留不可见频道事件，实际补发 %d 条", len(out))
	}
}
