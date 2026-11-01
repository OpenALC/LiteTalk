package signal

// token_recheck_test.go — 令牌会话存续复查（PR118 P2-2）：PAT/bot 连接建立后令牌
// 被吊销/过期，最迟在复查点之后的下一次心跳被服务端断连；store 故障 fail-open
// 不断连；JWT/dev 连接（无 TokenID）不复查。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// recheckHub 复查测试夹具：botHub 注入 checker 并把心跳/复查间隔压到 30ms
// （ticker 在连接建立时取 heartbeatEvery，必须在 dial 之前设，与 presence_retry
// 夹具同口径）。30ms 间隔下复查约每 2 拍命中一次。
func recheckHub(t *testing.T, chk TokenSessionChecker) *botHub {
	t.Helper()
	f := newBotHub(t)
	f.hub.sessionChecks = chk
	f.hub.heartbeatEvery = 30 * time.Millisecond
	f.hub.tokenRecheckEvery = 30 * time.Millisecond
	return f
}

// storeBackedChecker 复刻 main.go apitokenSessionChecker 的判定口径（吊销/过期/
// 行删 → false），供端到端验证「吊销 → 断连」全链路。
func storeBackedChecker(st *store.Mem) TokenSessionChecker {
	return checkerFunc(func(ctx context.Context, id int64) (bool, error) {
		row, err := st.APITokenByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		now := time.Now()
		if row.RevokedAt != nil && !now.Before(*row.RevokedAt) {
			return false, nil
		}
		if row.ExpiresAt != nil && !now.Before(*row.ExpiresAt) {
			return false, nil
		}
		return true, nil
	})
}

// checkerFunc 函数形态的 TokenSessionChecker。
type checkerFunc func(ctx context.Context, tokenID int64) (bool, error)

func (f checkerFunc) SessionValid(ctx context.Context, tokenID int64) (bool, error) {
	return f(ctx, tokenID)
}

// countingChecker 可编程桩：固定返回 valid/err，计数调用并记录每次收到的
// tokenID。fail-open 与 JWT 跳过用例靠计数/tokenID 证明「复查确实跑过、且只
// 为令牌主体跑」，避免空转假绿。
type countingChecker struct {
	mu    sync.Mutex
	valid bool
	err   error
	calls atomic.Int64
	ids   []int64
}

func (c *countingChecker) SessionValid(_ context.Context, tokenID int64) (bool, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.ids = append(c.ids, tokenID)
	c.mu.Unlock()
	return c.valid, c.err
}

// seenIDs 返回历次调用的 tokenID 快照。
func (c *countingChecker) seenIDs() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.ids...)
}

// waitClosed 等服务端断连：吊销路径走 closeNow（无 close 帧），客户端读到任意
// 读错误即算（TestHeartbeatClosesZombie 同口径）。单条长 Read——客户端在读的
// 时候才自动回 Pong，心跳节拍与复查才走得动。
func waitClosed(t *testing.T, c *wsClient) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
	defer cancel()
	_, _, err := c.conn.Read(ctx)
	if err == nil {
		t.Fatalf("waitClosed(user=%s): 未断连反而收到数据帧", c.user)
	}
}

// expectNoClose 让客户端连续读 d 时长并断言期间服务端不断连（fail-open 验收）。
// coder/websocket 的 Read 截止会关闭连接（setupReadTimeout → c.close），所以本
// 函数只能收尾使用，返回后不得再读写该连接；窗口内的 pings 由这次 Read 自动
// 回应，服务端心跳节拍与复查得以走动。服务端主动断连（含 closeNow 的异常断开）
// 会让 Read 提前返回，故以「读满窗口」为判据——截止时刻可能有在途 ping 撞上
// 连接关闭，错误形态不保证是 DeadlineExceeded，但返回时刻必然贴近窗口末端。
func expectNoClose(t *testing.T, c *wsClient, d time.Duration) {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, _, err := c.conn.Read(ctx)
	if err == nil {
		t.Fatalf("expectNoClose(user=%s): 不应收到数据帧", c.user)
	}
	if elapsed := time.Since(start); elapsed < d-60*time.Millisecond {
		t.Fatalf("expectNoClose(user=%s): 连接在窗口内被断开（%v 后返回，err=%v），不应发生", c.user, elapsed, err)
	}
}

// TestTokenRecheckClosesRevokedConn 核心验收（P2-2）：bot 连接在线时令牌被吊销，
// 下个复查点服务端主动断连，连接索引随 cleanup 清空——「吊销立即生效」从此对
// 已建立的 WS 连接也成立。
func TestTokenRecheckClosesRevokedConn(t *testing.T) {
	f := newBotHub(t)
	chk := storeBackedChecker(f.st)
	f.hub.sessionChecks = chk
	f.hub.heartbeatEvery = 30 * time.Millisecond
	f.hub.tokenRecheckEvery = 30 * time.Millisecond

	plain, _, tokenID := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	bot := f.dialBot(t, plain)

	if err := f.st.RevokeAPIToken(context.Background(), tokenID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	waitClosed(t, bot)
	waitConnsEmpty(t, f.hub)
}

// TestTokenRecheckFailOpenOnStoreError 复查查询故障不断连接（fail-open）：store
// 抖动不应误杀在线会话，下个复查点再查。断言复查确实执行过多拍（全部失败），
// 且整个窗口内服务端没有断连。
func TestTokenRecheckFailOpenOnStoreError(t *testing.T) {
	f := recheckHub(t, &countingChecker{err: errors.New("store down")})
	plain, _, _ := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	bot := f.dialBot(t, plain)

	// 300ms 窗口 ≥ 8 拍：客户端连续读（回 Pong），复查每拍失败，连接不断。
	expectNoClose(t, bot, 300*time.Millisecond)
	chk := f.hub.sessionChecks.(*countingChecker)
	if n := chk.calls.Load(); n < 3 {
		t.Fatalf("复查执行 %d 次，应 ≥3（证明故障路径真的被走到）", n)
	}
}

// TestTokenRecheckSkipsJWTConn JWT/dev 连接无 TokenID：复查不执行、连接不受
// 影响——JWT 主体不存在「吊销」概念，exp 由握手与旧令牌过渡期兜底。同 hub 里
// 放一条 bot 连接作对照：复查为它跑过且 tokenID 是 bot 的，证明心跳与复查机制
// 确实在跑，JWT 的零计数不是空转假绿。
func TestTokenRecheckSkipsJWTConn(t *testing.T) {
	f := recheckHub(t, &countingChecker{valid: false}) // 若对 JWT 误调用会立即断连
	plain, _, botTokenID := f.issueBotToken(t, []string{token.ScopeMessagesRead})
	bot := f.dialBot(t, plain)
	jwt := f.dial("1", "alice")

	// bot 侧连续读保住心跳节拍（它会被 valid=false 的复查正常关掉，无需断言）；
	// jwt 侧读满 300ms 窗口：服务端始终没有对它断连。
	botDone := make(chan struct{})
	go func() {
		defer close(botDone)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, _, _ = bot.conn.Read(ctx)
	}()
	expectNoClose(t, jwt, 300*time.Millisecond)
	<-botDone

	chk := f.hub.sessionChecks.(*countingChecker)
	if n := chk.calls.Load(); n < 1 {
		t.Fatal("复查未执行（对照机制空转）")
	}
	for _, id := range chk.seenIDs() {
		if id != botTokenID {
			t.Fatalf("复查出现了非 bot 令牌的 tokenID %d（应为 %d）", id, botTokenID)
		}
	}
}
