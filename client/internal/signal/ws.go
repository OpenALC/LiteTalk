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
// Close 或鉴权失败（ErrAuthFatal）。其余错误只会触发重连，不会返回。
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.ReconnectMin
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

		if c.Stats.Connects.Add(1) > 1 {
			c.Stats.Reconnects.Add(1)
		}
		err = c.serve(ctx, conn)
		c.setConn(nil)
		_ = conn.CloseNow()
		if c.closed.Load() || ctx.Err() != nil {
			return nil
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

// serve 跑一条连接的生命周期：读循环分发 + 心跳，直到出错返回。
func (c *Client) serve(ctx context.Context, conn *websocket.Conn) error {
	conn.SetReadLimit(1 << 20) // hello 携带全量数据，放宽到 1MB
	c.setConn(conn)
	c.logf("connected to %s", c.cfg.URL)

	// 心跳：服务端 Redis presence TTL 90s，默认 30s 一拍。
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		t := time.NewTicker(c.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
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
	defer func() { <-pingDone }() // 等心跳 goroutine 退出（conn 关闭后 Ping 必然失败返回）

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
		c.dispatch(env)
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
