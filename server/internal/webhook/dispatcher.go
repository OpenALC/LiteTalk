package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/linkpreview"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// Config 投递参数。零值字段一律回落安全默认；Wait/Now/CheckURL 仅供测试注入
// （退避曲线不 sleep 真实时间、SSRF 校验放行 httptest 本地目标）。
type Config struct {
	// RetryMax 最多重试次数（不含首发）；负数视为 0。
	RetryMax int
	// BackoffBase 退避基数：第 k 次重试前等待 min(base*2^(k-1), MaxBackoff)。
	BackoffBase time.Duration
	// Timeout 单次投递整体超时。
	Timeout time.Duration
	// Workers worker 数；<=0 回落 DefaultWorkers。
	Workers int
	// Queue 派发缓冲条数；<=0 回落 DefaultQueue。
	Queue int
	// BreakerAt 连续失败熔断阈值；<=0 回落 DefaultBreakerAt。
	BreakerAt int
	// Retention 投递记录保留期；<=0 回落 DefaultRetention。
	Retention time.Duration
	// CheckURL 出站目标 SSRF 校验：创建/更新时与投递（含每一跳重定向）前调用。
	// nil 用 linkpreview.CheckDialable。
	CheckURL func(rawURL string) error
	// Visibility 投递前频道可见性判定（P1-5）：频道维度事件只投给「订阅创建者
	// 当前对该频道有 ViewChannel」的订阅。nil = fail-closed（频道维度事件一律
	// 跳过）——生产装配（cmd/server）恒注入 authz.Authorizer；nil 只应出现在
	// 未接线的测试里，绝不允许以 nil 上线。
	Visibility VisibilityChecker
	// Now 时钟；nil 用 time.Now。
	Now func() time.Time
	// Wait 退避等待；nil 用真实 timer。返回非 nil（含 ctx 取消）即放弃重试。
	Wait func(ctx context.Context, d time.Duration) error
	// Log 为 nil 用 log.Default()。
	Log *log.Logger
}

func (c *Config) fill() {
	if c.RetryMax < 0 {
		c.RetryMax = 0
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = DefaultBackoffBase
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.Queue <= 0 {
		c.Queue = DefaultQueue
	}
	if c.BreakerAt <= 0 {
		c.BreakerAt = DefaultBreakerAt
	}
	if c.Retention <= 0 {
		c.Retention = DefaultRetention
	}
	if c.CheckURL == nil {
		c.CheckURL = linkpreview.CheckDialable
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Wait == nil {
		c.Wait = defaultWait
	}
	if c.Log == nil {
		c.Log = log.Default()
	}
}

// defaultWait 真实退避等待：ctx 取消立即返回（进程退出不等一个 5 分钟的退避）。
func defaultWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Store 投递器依赖的存储窄接口（*store.Store 满足；测试注入 Mem）。
type Store interface {
	ChannelByID(ctx context.Context, id int64) (*store.Channel, error)
	ServerByID(ctx context.Context, id int64) (*store.Server, error)
	OutboundWebhooksOfServer(ctx context.Context, serverID int64) ([]store.OutboundWebhook, error)
	RecordWebhookDelivery(ctx context.Context, d *store.WebhookDelivery, breakerAt int, reason string) error
	WebhookDeliveriesBefore(ctx context.Context, before time.Time, limit int) ([]store.WebhookDelivery, error)
	DeleteWebhookDeliveries(ctx context.Context, ids []int64) error
}

// VisibilityChecker 投递前频道可见性判定的窄接口（*authz.Authorizer 满足）。
// 走纯角色判定（RoleCanInServer）而非 CanInServer：订阅创建者是账号而非凭据，
// 其频道可见性只由角色/覆盖决定——scope 交集是令牌主体的 API 能力上限，对
// 「谁会看到这条消息的投递」没有意义（PR118 P1-5）。
type VisibilityChecker interface {
	RoleCanInServer(ctx context.Context, uid, serverID, channelID int64, perm store.Permission) (bool, error)
}

// outboundTask 一次入队单元：出口只给事件与上下文，订阅匹配与投递全在 worker
// （绝不阻塞广播/请求路径）。serverID 非 0 时直接采用（REST 事件源已归属到服，
// 且 channel.deleted 的频道行此刻已删，不能走频道反查）；为 0 时 worker 经
// channelID 反查（hub 的 message.created 路径）。
type outboundTask struct {
	event     string
	channelID string
	serverID  int64
	payload   json.RawMessage
}

// Dispatcher 出站投递器：带缓冲 channel + worker 池。实现 signal.WebhookOutlet
// （OnWebhookEvent 只做非阻塞入队）。
//
// 零订阅零 goroutine：Start/EnsureStarted 之前不启动任何后台协程；无订阅的
// 部署里没人调用它们，事件出口见未启动直接丢弃。
type Dispatcher struct {
	cfg    Config
	store  Store
	client *http.Client
	log    *log.Logger

	started   atomic.Bool
	startOnce sync.Once
	ch        chan outboundTask
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// New 构造投递器（不启动 goroutine）。store 为 nil 返回 nil（无法投递，调用方
// 按功能关闭处理）。
func New(cfg Config, st Store) *Dispatcher {
	if st == nil {
		return nil
	}
	cfg.fill()
	d := &Dispatcher{
		cfg:   cfg,
		store: st,
		log:   cfg.Log,
		ch:    make(chan outboundTask, cfg.Queue),
	}
	// 出站 HTTP client：整体超时（一个慢目标最多占一个 worker 一个超时周期）、
	// 响应体不读（见 postOnce）、禁环境代理与 keep-alive（代理会绕过拨号层校验，
	// 复用连接会让逐次拨号校验失效）。
	d.client = &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("webhook: 重定向次数过多")
			}
			// 每一跳复检 URL 形状；解析出的 IP 在下一跳 safeDial 里再校验一次。
			if err := cfg.CheckURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            d.safeDial,
			DisableKeepAlives:      true,
			ResponseHeaderTimeout:  cfg.Timeout,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			MaxResponseHeaderBytes: 64 << 10,
		},
	}
	return d
}

// safeDial 拨号前解析主机名并对每个解析结果过 SSRF 校验，通过后直接向该 IP 拨号，
// 杜绝「校验用一次解析、拨号再解析」的 DNS 重绑定窗口。URL 可能在创建后经 DNS
// 被改指内网——这是投递前的最后一道闸（创建/更新时的 CheckURL 只是静态判定）。
// IP 校验经 CheckURL("http://<ip>:<port>") 复用：两个 scheme 在校验里等价
// （都只允许 http/https，IP 黑名单与 scheme 无关）。
func (d *Dispatcher) safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("webhook: 目标地址不合法: %w", err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("webhook: 解析 %s 失败", host)
	}
	for _, ipAddr := range ips {
		if err := d.cfg.CheckURL("http://" + net.JoinHostPort(ipAddr.IP.String(), port)); err != nil {
			return nil, fmt.Errorf("webhook: 目标解析到被禁地址")
		}
	}
	var dialer net.Dialer
	var lastErr error
	for _, ipAddr := range ips {
		conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = errors.New("无可用地址")
	}
	return nil, fmt.Errorf("webhook: 拨号失败: %w", lastErr)
}

// Start 启动 worker 与投递记录清理 goroutine（幂等）。main 启动期库内已有订阅时
// 以服务端 shutdown ctx 调用（清理 goroutine 随之干净停止）；API 首次创建订阅时
// 传 nil（Background，由 Close 收口）。ctx 为 nil 视为 context.Background()。
func (d *Dispatcher) Start(ctx context.Context) {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		runCtx, cancel := context.WithCancel(ctx)
		d.cancel = cancel
		d.wg.Add(d.cfg.Workers + 1)
		for i := 0; i < d.cfg.Workers; i++ {
			go d.worker(runCtx)
		}
		go d.cleaner(runCtx)
		d.started.Store(true)
		d.log.Printf("[webhook] 投递器已启动（workers=%d，retry_max=%d，backoff_base=%s，timeout=%s）",
			d.cfg.Workers, d.cfg.RetryMax, d.cfg.BackoffBase, d.cfg.Timeout)
	})
}

// EnsureStarted 惰性启动（API 创建首个订阅时用；等价 Start(nil)）。
func (d *Dispatcher) EnsureStarted() { d.Start(nil) }

// Close 停止 worker 与清理 goroutine 并等待退出；未启动过 no-op。
func (d *Dispatcher) Close() {
	if d == nil || d.cancel == nil {
		return
	}
	d.cancel()
	d.wg.Wait()
}

// OnWebhookEvent 出站事件出口（signal.WebhookOutlet）：只做一次非阻塞入队，
// 绝不阻塞 WS 写路径。未启动（零订阅部署）直接丢弃；队列满丢弃并记日志——
// 出站通知是尽力而为的旁路，宁可丢事件也不能拖住信令。
func (d *Dispatcher) OnWebhookEvent(eventType, channelID string, payload json.RawMessage) {
	if d == nil || !d.started.Load() {
		return
	}
	select {
	case d.ch <- outboundTask{event: eventType, channelID: channelID, payload: payload}:
	default:
		d.log.Printf("[webhook] 派发队列已满（%d），丢弃 %s 事件", cap(d.ch), eventType)
	}
}

// OnServerEvent REST 层出站事件派发（#52）：member/channel 事件的触发点在 REST
// 成功路径上，不在 hub 事件流里，由各成功点直接调用。服务器归属由调用方给出
// （member 事件没有频道；channel.deleted 的行已删，反查会丢事件）；channelID 为
// 0 表示事件无频道维度（投递体不带 channel_id）。入队语义与 OnWebhookEvent 相同。
func (d *Dispatcher) OnServerEvent(eventType string, serverID, channelID int64, payload json.RawMessage) {
	if d == nil || !d.started.Load() {
		return
	}
	chID := ""
	if channelID != 0 {
		chID = strconv.FormatInt(channelID, 10)
	}
	select {
	case d.ch <- outboundTask{event: eventType, serverID: serverID, channelID: chID, payload: payload}:
	default:
		d.log.Printf("[webhook] 派发队列已满（%d），丢弃 %s 事件", cap(d.ch), eventType)
	}
}

// worker 消费派发队列。
func (d *Dispatcher) worker(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-d.ch:
			d.process(ctx, t)
		}
	}
}

// process 单个事件的完整投递链：服务器归属解析 → 订阅匹配 → 逐订阅带退避投递。
func (d *Dispatcher) process(ctx context.Context, t outboundTask) {
	serverID := t.serverID
	if serverID == 0 {
		chID, err := strconv.ParseInt(t.channelID, 10, 64)
		if err != nil {
			return // dev 内置房间无 store 实体，不出站
		}
		ch, err := d.store.ChannelByID(ctx, chID)
		if err != nil {
			return // 频道不存在（并发删除）或 store 故障：丢弃
		}
		serverID = ch.ServerID
	}
	if serverID == 0 {
		return // 私聊/群聊会话无服务器归属，出站订阅只挂服务器
	}
	hooks, err := d.store.OutboundWebhooksOfServer(ctx, serverID)
	if err != nil {
		d.log.Printf("[webhook] 读取服务器 %d 订阅失败: %v", serverID, err)
		return
	}
	body, err := json.Marshal(outboundEnvelope{
		Event:     t.event,
		ServerID:  strconv.FormatInt(serverID, 10),
		ChannelID: t.channelID,
		Data:      t.payload,
	})
	if err != nil {
		return
	}
	// 投递前按创建者频道可见性过滤（P1-5）。ownerID 懒加载：仅存在 creator_id
	// 为空的旧行时才回查服务器行，单次 process 至多查一次，多个空 creator 订阅共享。
	var ownerID int64
	for i := range hooks {
		w := hooks[i]
		if w.Disabled || !subscribed(w.Events, t.event) {
			continue
		}
		if !d.creatorVisible(ctx, t, w, serverID, &ownerID) {
			continue
		}
		d.deliver(ctx, w, t.event, body)
	}
}

// creatorVisible 投递前可见性闸（PR118 P1-5）。频道维度的白名单事件
// （message.created / channel.created / channel.deleted）只投给「判定主体当前
// 对该频道有 ViewChannel」的订阅；member.* 无频道维度且载荷不含频道内容，不过此闸。
//
// 判定主体：订阅创建者（CreatorID）；0 = 旧行/未记创建者——按服务器 owner 代持
// （0022 已把存量行回填成 owner_id，这里是兜底：回填时服务器已删的孤儿行），
// owner 也不可考（服务器行已删）时 fail-closed 跳过。
//
// 判定口径：纯角色（RoleCanInServer，不做 scope 交集）——创建者是账号而非凭据，
// 可见性随角色/频道覆盖实时变化，因此每条事件现查而不缓存结论；authz 快照有
// TTL 缓存与单飞，同服多订阅、同服高频事件共享一次加载，无 N+1（量级：每事件
// 每订阅一次纯内存合并，至多每 TTL 每服务器一次 Store 快照加载）。
//
// 跳过语义：判定不过或不可判（含判定器故障、fail-closed）一律静默跳过——不算
// 投递失败、不进重试、不推熔断计数；只留一条日志（仅 id 级信息，不含消息内容）。
func (d *Dispatcher) creatorVisible(ctx context.Context, t outboundTask, w store.OutboundWebhook, serverID int64, ownerID *int64) bool {
	if t.channelID == "" {
		return true // member.* 无频道维度：无可判定的可见性
	}
	chID, err := strconv.ParseInt(t.channelID, 10, 64)
	if err != nil {
		// serverID != 0 的路径 channelID 必为十进制（OnServerEvent/OnWebhookEvent
		// 都从 int64 格式化）；到不了这里说明上游改了契约，按 fail-closed 处理。
		d.log.Printf("[webhook] 跳过订阅 %d：事件 %s 的频道 id 非法 %q", w.ID, t.event, t.channelID)
		return false
	}
	uid := w.CreatorID
	if uid == 0 {
		if *ownerID == 0 {
			srv, err := d.store.ServerByID(ctx, serverID)
			if err != nil {
				d.log.Printf("[webhook] 跳过订阅 %d：creator 为空且服务器 %d 不可考: %v", w.ID, serverID, err)
				return false
			}
			*ownerID = srv.OwnerID
		}
		uid = *ownerID
	}
	if uid == 0 {
		d.log.Printf("[webhook] 跳过订阅 %d：creator 与服务器 %d 的 owner 均不可考", w.ID, serverID)
		return false
	}
	if d.cfg.Visibility == nil {
		d.log.Printf("[webhook] 跳过订阅 %d：可见性判定器未注入（fail-closed）", w.ID)
		return false
	}
	ok, err := d.cfg.Visibility.RoleCanInServer(ctx, uid, serverID, chID, store.ViewChannel)
	if err != nil {
		// 判定故障按「不可判」处理：绝不把故障伪装成有权限（与 authz 同取向）。
		d.log.Printf("[webhook] 跳过订阅 %d：可见性判定失败（channel %d）: %v", w.ID, chID, err)
		return false
	}
	if !ok {
		d.log.Printf("[webhook] 跳过订阅 %d：创建者 %d 对频道 %d 无查看权限，%s 不投递", w.ID, uid, chID, t.event)
		return false
	}
	return true
}

// deliver 单订阅投递：2xx 成功；4xx（除 429）立即放弃；5xx 与超时/网络错误按
// base*2^(k-1)（封顶 MaxBackoff）退避重试至多 RetryMax 次。最终结果落投递记录
// 并推进熔断计数（成功清零、失败自增，达阈值由 Store 停用）。
func (d *Dispatcher) deliver(ctx context.Context, w store.OutboundWebhook, event string, body []byte) {
	// 投递前静态复检：URL 的 DNS 可能在创建后改指内网，拨号层是第二道闸，
	// 这里先挡掉能直接判定的（字面量内网 IP / localhost / 变更后的非法 scheme）。
	if err := d.cfg.CheckURL(w.URL); err != nil {
		d.record(ctx, w.ID, event, 0, false, 0, "ssrf_blocked")
		return
	}
	attempts := 0
	lastCode, lastErr := 0, ""
	for {
		attempts++
		ts := d.cfg.Now().Unix()
		code, err := d.postOnce(ctx, w, event, ts, body)
		if err == nil && code >= 200 && code < 300 {
			d.record(ctx, w.ID, event, attempts, true, code, "")
			return
		}
		// postOnce 对「HTTP 往返完成」返回 (code, nil)，成败要按状态码分；
		// err 非 nil 才是超时/网络层失败。
		if err != nil {
			lastCode, lastErr = code, err.Error()
		} else {
			lastCode, lastErr = code, fmt.Sprintf("HTTP %d", code)
		}
		if !retryable(code, err) || attempts > d.cfg.RetryMax {
			break
		}
		if werr := d.cfg.Wait(ctx, backoff(d.cfg.BackoffBase, attempts)); werr != nil {
			lastErr = werr.Error()
			break // ctx 取消（进程退出）：放弃重试，落失败记录
		}
	}
	d.record(ctx, w.ID, event, attempts, false, lastCode, lastErr)
}

// backoff 第 k 次重试前的等待：base*2^(k-1)，封顶 MaxBackoff。
func backoff(base time.Duration, k int) time.Duration {
	d := base
	for i := 1; i < k && d < MaxBackoff; i++ {
		d *= 2
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}

// retryable 是否值得重试：网络/超时错误（code=0）与 429、5xx 重试；
// 其余 4xx（目标明确拒绝）与 3xx 立即放弃。
func retryable(code int, err error) bool {
	if err != nil {
		return true
	}
	if code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500
}

// postOnce 单次投递。响应体一律不读（只关）——读多少都会给慢目标留拖住 worker
// 的口子；client.Timeout 已覆盖整体耗时。
func (d *Dispatcher) postOnce(ctx context.Context, w store.OutboundWebhook, event string, ts int64, body []byte) (int, error) {
	rctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "LiteTalk-Webhook/1.0")
	req.Header.Set("X-LiteTalk-Event", event)
	req.Header.Set("X-LiteTalk-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-LiteTalk-Signature", "sha256="+Sign(w.Secret, ts, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() // 不读 body：见函数注释
	return resp.StatusCode, nil
}

// record 落投递记录并推进熔断计数。Store 故障只记日志——投递本身已完成，
// 不能为一条记录重投整个事件。
func (d *Dispatcher) record(ctx context.Context, hookID int64, event string, attempts int, ok bool, code int, errMsg string) {
	err := d.store.RecordWebhookDelivery(ctx, &store.WebhookDelivery{
		WebhookID:  hookID,
		Event:      event,
		Attempts:   attempts,
		OK:         ok,
		StatusCode: code,
		Error:      truncateRunes(errMsg, deliveryErrorMax),
	}, d.cfg.BreakerAt, DisabledReason)
	if err != nil {
		d.log.Printf("[webhook] 落投递记录失败（hook %d，%s）: %v", hookID, event, err)
	}
}

// cleaner 投递记录清理：每 sweepEvery 扫一批早于保留期的记录删除。ctx 取消
// （服务端 shutdown / Close）即退出——必须随进程干净停止，不留裸协程。
func (d *Dispatcher) cleaner(ctx context.Context) {
	defer d.wg.Done()
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sweepOnce(ctx)
		}
	}
}

// sweepOnce 清一轮过期投递记录（早于 now-Retention）。批量循环直到清完或出错；
// 导出为包内方法供测试直接调用（不必等一个 sweep 周期）。
func (d *Dispatcher) sweepOnce(ctx context.Context) {
	before := d.cfg.Now().Add(-d.cfg.Retention)
	for {
		rows, err := d.store.WebhookDeliveriesBefore(ctx, before, 500)
		if err != nil {
			d.log.Printf("[webhook] 扫描过期投递记录失败: %v", err)
			return
		}
		if len(rows) == 0 {
			return
		}
		ids := make([]int64, 0, len(rows))
		for i := range rows {
			ids = append(ids, rows[i].ID)
		}
		if err := d.store.DeleteWebhookDeliveries(ctx, ids); err != nil {
			d.log.Printf("[webhook] 删除过期投递记录失败: %v", err)
			return
		}
		if len(rows) < 500 {
			return
		}
	}
}

// truncateRunes 按 rune 截断到 n 个字符（VARCHAR(255) 按字符计）。
func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
