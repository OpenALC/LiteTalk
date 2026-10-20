package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ErrNotConnected 在无可用连接时由 Send 返回（重连窗口期）。
var ErrNotConnected = errors.New("signal: not connected")

// ErrAuthFatal 鉴权失败（HTTP 401/403），Run 直接返回，不再重连。
var ErrAuthFatal = errors.New("signal: authentication failed")

// ErrAuthTransient 由 OnAuthFailed 返回，表示刷新令牌的请求本身（传输层）
// 失败了——网络中断、DNS、超时等，服务端并未拒绝刷新令牌。令牌未必失效，
// 因此 Run 不按致命收场：复位刷新预算、按重连退避后再来一轮（#84 复审 4.1）。
var ErrAuthTransient = errors.New("signal: token refresh failed transiently")

// ErrSilentReplace 同 sid 的半开连接被服务端的新连接替换（WS close 4000，
// 契约 §0.6）。服务端那条新连接已经跑起来，本端重连只会把它再顶掉、互相踢，
// 因此 Run 直接返回，由上层决定后续（通常是跟着一次新的登录流程）。
var ErrSilentReplace = errors.New("signal: connection replaced by a newer session")

// Handler 信令消息处理器。在读循环 goroutine 上同步调用，必须快速、不阻塞。
type Handler func(Envelope)

// Config WS 客户端配置。零值字段回落为默认值。
type Config struct {
	URL string // ws(s)://host/ws（不含 query）
	// TokenFunc 每次拨号前调用取鉴权令牌（追加为 ?token=...；dev 模式为 dev-<name>）。
	// 用函数而非静态串：REST 侧刷新令牌后，重连自动带上新令牌。nil = 不带 token。
	TokenFunc func(ctx context.Context) (string, error)

	PingInterval time.Duration // 心跳周期，默认 30s（服务端 presence TTL 90s）
	WriteTimeout time.Duration // 单条写超时，默认 10s
	DialTimeout  time.Duration // 握手超时，默认 10s
	ReconnectMin time.Duration // 重连退避下限，默认 500ms
	ReconnectMax time.Duration // 重连退避上限，默认 15s

	// OnConnect 每次（重）连成功并收到首条消息后回调，业务应在此重发
	// channel.join 等订阅状态。在内部 goroutine 上调用，可安全 Send。
	OnConnect func(c *Client)
	// OnDisconnect 连接断开（准备重连或退出）时回调。
	OnDisconnect func(err error)
	// OnKicked 被服务端踢下线时回调（契约 §0.5/§0.6）。session.kicked 消息
	// 是「尽力而为」，发送窗口内丢帧就什么都不剩，因此 close 4001 与握手 401
	// 的 session_replaced/session_replayed 头是可靠的那一半——本回调就是兜底。
	// reason 约定值见 KickReasonLoginElsewhere。在 Run 的 goroutine 上同步调用，
	// 实现侧不得同步 Close 本客户端（Close 会等 Run 退出，自成死锁）。
	OnKicked func(reason string)
	// OnAuthFailed 握手 401（被顶替类除外）时回调，供上层刷新 access 令牌。
	// 返回 nil = 已刷新，Run 立即重拨一次（不叠加退避）；返回 ErrAuthTransient =
	// 刷新请求是传输层失败（令牌本身未必失效），Run 复位刷新预算、按普通断线
	// 退避后继续重连；返回其他非 nil = 刷新被服务端明确拒绝或不可恢复，Run 以
	// ErrAuthFatal 返回，不再重连——宁可回登录态，也不拿旧令牌无限撞墙。
	// 每次连接生命周期只有一次重试预算：刷新后仍被拒即停（瞬时失败不计入预算）。
	// 被顶替类 401 走 OnKicked，不经过本回调。nil = 不刷新（旧行为）。
	OnAuthFailed func(ctx context.Context) error
	// Gate 事件面排序/去重门（#32）：对每条下行信封返回应投递给订阅者的信封
	//（按返回序投递；通常 0 或 1 条，补发窗口的乱序回填可能一次多条）。
	// nil = 全部直通（老路径）。gateway.Tracker.Gate 是标准实现。
	Gate func(Envelope) []Envelope
	// Logger 为 nil 时静默。
	Logger *log.Logger
}

func (c *Config) defaults() {
	if c.PingInterval == 0 {
		c.PingInterval = 30 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.ReconnectMin == 0 {
		c.ReconnectMin = 500 * time.Millisecond
	}
	if c.ReconnectMax == 0 {
		c.ReconnectMax = 15 * time.Second
	}
}

// Stats 运行统计（原子计数）。
type Stats struct {
	Connects         atomic.Uint64 // 成功建立连接次数（含重连）
	Reconnects       atomic.Uint64 // 重连次数（Connects-1）
	MessagesSent     atomic.Uint64
	MessagesReceived atomic.Uint64
	SendErrors       atomic.Uint64
}

// Client WebSocket 信令客户端：自动重连（指数退避）、心跳、按类型分发。
//
// 使用：NewClient → On 订阅 → Run（阻塞直到 Close/ctx 取消/鉴权失败）。
type Client struct {
	cfg Config
	log *log.Logger

	handlersMu sync.RWMutex
	handlers   map[string][]Handler

	connMu sync.Mutex
	conn   *websocket.Conn // nil 表示处于重连窗口

	closed atomic.Bool
	done   chan struct{}

	Stats Stats
}

// NewClient 创建客户端（尚未连接）。
func NewClient(cfg Config) *Client {
	cfg.defaults()
	if cfg.Logger == nil {
		cfg.Logger = log.New(nopWriter{}, "", 0)
	}
	return &Client{
		cfg:      cfg,
		log:      cfg.Logger,
		handlers: make(map[string][]Handler),
		done:     make(chan struct{}),
	}
}

// On 订阅某类型的消息。可在 Run 之前或之中调用。
func (c *Client) On(msgType string, h Handler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers[msgType] = append(c.handlers[msgType], h)
}

// Send 发送一条信令消息。无连接时返回 ErrNotConnected（调用方自行决定是否重试）。
func (c *Client) Send(msgType string, data any) error {
	env, err := NewEnvelope(msgType, data)
	if err != nil {
		return fmt.Errorf("signal: marshal %s: %w", msgType, err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("signal: marshal envelope: %w", err)
	}
	conn := c.currentConn()
	if conn == nil {
		c.Stats.SendErrors.Add(1)
		return ErrNotConnected
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.WriteTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.Stats.SendErrors.Add(1)
		return fmt.Errorf("signal: write %s: %w", msgType, err)
	}
	c.Stats.MessagesSent.Add(1)
	return nil
}

// Connected 报告当前是否有活跃连接。
func (c *Client) Connected() bool { return c.currentConn() != nil }

// Closed 报告客户端是否已被 Close（与「暂时断线、等待重连」区分）。
func (c *Client) Closed() bool { return c.closed.Load() }

// Close 停止客户端（幂等）。Run 将返回 nil。
func (c *Client) Close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
		c.connMu.Lock()
		if c.conn != nil {
			_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
		}
		c.connMu.Unlock()
	}
}

// Run 连接并维持信令会话：断线指数退避自动重连，直到 ctx 取消、
// Close、鉴权失败（ErrAuthFatal）或被新会话替换（ErrSilentReplace）。
// 其余错误只会触发重连，不会返回——包括 OnAuthFailed 报告的瞬时刷新失败
// （ErrAuthTransient，见该字段注释）。
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.ReconnectMin
	// authRetried 一条连接的 401 刷新重试预算：拨号成功即复位（下一条连接
	// 又有一次预算），刷新后仍 401 则保持 true 直接致命，杜绝握手 401 风暴。
	authRetried := false
	for {
		if c.closed.Load() {
			return nil
		}
		dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
		wsURL, terr := c.wsURL(dialCtx)
		cancel()
		if terr != nil {
			c.logf("token fetch failed: %v (retry in %v)", terr, backoff)
			if werr := c.wait(ctx, backoff); werr != nil {
				return nil
			}
			backoff = nextBackoff(backoff, c.cfg.ReconnectMax)
			continue
		}
		dialCtx, cancel = context.WithTimeout(ctx, c.cfg.DialTimeout)
		conn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{})
		cancel()
		if err != nil {
			if isAuthFailure(resp) {
				// 被踢类 401（session_replaced/session_replayed）必须先走被踢链路：
				// 只打日志返 ErrAuthFatal 的话，用户看到的只是「登录后一片空白」。
				if reason := kickedFromHeader(resp); reason != "" {
					c.logf("auth rejected: %s (HTTP %d)", reason, resp.StatusCode)
					c.fireKicked(reason)
					return ErrAuthFatal
				}
				// 普通 401 = 令牌过期/被吊销：刷新一次后立即重拨。刷新失败
				// 或重拨仍被拒都按致命收场，只打日志不重连会让常驻托盘的用户
				// 在 access 到期后再也连不上（#84）。
				if resp.StatusCode == http.StatusUnauthorized && !authRetried && c.cfg.OnAuthFailed != nil {
					authRetried = true
					c.logf("auth failed (HTTP %d), refreshing access token", resp.StatusCode)
					rerr := c.cfg.OnAuthFailed(ctx)
					if rerr == nil {
						continue
					}
					if ctx.Err() != nil || c.closed.Load() {
						return nil
					}
					if errors.Is(rerr, ErrAuthTransient) {
						// 刷新请求没到服务端（网络抖动/超时）：刷新令牌未必失效，
						// 按致命收场会让常驻托盘的用户在网络恢复后也永久离线。
						// 复位预算，按普通断线的退避节奏再来一轮；退避是硬约束，
						// 否则这条路会变成握手 401 紧循环（#84 复审 4.1）。
						authRetried = false
						c.logf("token refresh transiently failed: %v (retry in %v)", rerr, backoff)
						if werr := c.wait(ctx, backoff); werr != nil {
							return nil
						}
						backoff = nextBackoff(backoff, c.cfg.ReconnectMax)
						continue
					}
					c.logf("token refresh failed: %v, giving up", rerr)
					return ErrAuthFatal
				}
				c.logf("auth failed (HTTP %d), giving up", resp.StatusCode)
				return ErrAuthFatal
			}
			c.logf("dial failed: %v (retry in %v)", err, backoff)
			if werr := c.wait(ctx, backoff); werr != nil {
				return nil
			}
			backoff = nextBackoff(backoff, c.cfg.ReconnectMax)
			continue
		}
		authRetried = false

		if c.Stats.Connects.Add(1) > 1 {
			c.Stats.Reconnects.Add(1)
		}
		err = c.serve(ctx, conn)
		c.setConn(nil)
		_ = conn.CloseNow()
		if c.closed.Load() || ctx.Err() != nil {
			return nil
		}
		// close code 只在连接自然断开时判定：Close/取消 ctx 会带上
		// StatusNormalClosure 或 StatusAbnormalClosure，掺和进来会误判。
		if code := websocket.CloseStatus(err); code != -1 {
			switch code {
			case CloseCodeNewLogin:
				c.fireKicked(KickReasonLoginElsewhere)
			case CloseCodeSessionReplaced:
				c.logf("close %d: same-session connection replaced, not reconnecting", code)
				return ErrSilentReplace
			}
		}
		if c.cfg.OnDisconnect != nil {
			c.cfg.OnDisconnect(err)
		}
		c.logf("disconnected: %v (reconnect in %v)", err, backoff)
		if werr := c.wait(ctx, backoff); werr != nil {
			return nil
		}
		backoff = nextBackoff(backoff, c.cfg.ReconnectMax)
	}
}

// fireKicked 触发 OnKicked（未注册时空转）。
func (c *Client) fireKicked(reason string) {
	if c.cfg.OnKicked != nil {
		c.cfg.OnKicked(reason)
	}
}

// serve 跑一条连接的生命周期：读循环分发 + 心跳，直到出错返回。
func (c *Client) serve(ctx context.Context, conn *websocket.Conn) error {
	conn.SetReadLimit(1 << 20) // hello 携带全量数据，放宽到 1MB
	c.setConn(conn)
	c.logf("connected to %s", c.cfg.URL)

	// 协议版本协商（#29）：连上立刻声明可接受区间，再进读循环。
	// 老服务端不认识 client.hello 会回 error{bad_message}——上层容忍、按 v1 继续，
	// 不因协商失败重连。发不出去说明连接已废，返回错误走正常断线重连。
	if err := c.Send(TypeClientHello, ClientHelloData{
		Min: ProtocolVersionMin, Max: ProtocolVersionMax,
	}); err != nil {
		return fmt.Errorf("client.hello: %w", err)
	}

	// 心跳：服务端 Redis presence TTL 90s，默认 30s 一拍。
	// serveCtx 在本连接读循环退出时取消，让心跳 goroutine 立即退出——否则
	// 连接已死却要等下一个 Ping tick（默认 30s）才解除阻塞，重连被无谓拖慢
	//（#32 断线续传依赖快速重连；Wi-Fi/4G 切换场景尤其明显）。
	serveCtx, stopPing := context.WithCancel(ctx)
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		t := time.NewTicker(c.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-c.done:
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(context.Background(), c.cfg.PingInterval/2)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				c.logf("ping failed: %v", err)
				_ = conn.CloseNow() // 让读循环退出，进入重连
				return
			}
		}
	}()
	defer func() { stopPing(); <-pingDone }() // 读循环退出即放行重连，不等下一个 tick

	first := true
	for {
		// 读超时由心跳兜底（对端不 pong 则 Ping 超时关连接），此处不带 deadline。
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.logf("drop malformed message: %v", err)
			continue
		}
		c.Stats.MessagesReceived.Add(1)
		// Gate（#32）：事件面消息去重/重排后投递；一次可返回多条（乱序回填）。
		if c.cfg.Gate != nil {
			for _, out := range c.cfg.Gate(env) {
				c.dispatch(out)
			}
		} else {
			c.dispatch(env)
		}
		if first {
			first = false
			if c.cfg.OnConnect != nil {
				c.cfg.OnConnect(c)
			}
		}
	}
}

// dispatch 按类型分发给订阅者（读循环上同步执行，保序）。
func (c *Client) dispatch(env Envelope) {
	c.handlersMu.RLock()
	hs := c.handlers[env.Type]
	c.handlersMu.RUnlock()
	for _, h := range hs {
		h(env)
	}
}

func (c *Client) currentConn() *websocket.Conn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

// wait 等待 d 或直到关闭/取消；返回非 nil 表示应退出 Run。
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.done:
		return errors.New("closed")
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) wsURL(ctx context.Context) (string, error) {
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return c.cfg.URL, nil // Dial 会再报一次错
	}
	if c.cfg.TokenFunc != nil {
		tok, err := c.cfg.TokenFunc(ctx)
		if err != nil {
			return "", err
		}
		if tok != "" {
			q := u.Query()
			q.Set("token", tok)
			u.RawQuery = q.Encode()
		}
	}
	return u.String(), nil
}

func (c *Client) logf(format string, args ...any) {
	c.log.Printf("signal: "+format, args...)
}

// authHeader 鉴权失败原因响应头：握手 401 与 REST 401 都会带上
// （服务端 signal/hub.go 与 api/router.go 写入）。客户端据此区分
// 「该静默刷新令牌」（token_expired）与「本端已被踢」（session_replaced/
// session_replayed）——后者必须走被踢链路，否则用户看到的是静默的登录失败。
const authHeader = "X-LiteTalk-Auth"

// kickedFromHeader 从 401 响应解析被踢原因；非被踢类（token_expired、缺头）
// 返回空串，由调用方按普通鉴权失败处理。
func kickedFromHeader(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	switch resp.Header.Get(authHeader) {
	case "session_replaced", "session_replayed":
		return KickReasonLoginElsewhere
	}
	return ""
}

// isAuthFailure 通过 Dial 返回的 HTTP 响应判断鉴权失败（401/403）。
func isAuthFailure(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

// nextBackoff 指数退避 ×2，封顶 max，加 ±20% 抖动防雷群。
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	jitter := time.Duration(rand.Int63n(int64(next) / 5))
	return next - next/10 + jitter
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
