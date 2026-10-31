package push

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 节流与队列参数。
const (
	// pushThrottleKey 同一接收者 10 分钟最多 1 条推送（跨触发类型共享，防刷屏）。
	// 键就是契约要求的 push:<uid>。
	pushThrottleKey   = "push:"
	pushThrottleTTL   = 10 * time.Minute
	pushThrottleLimit = 1
	// srcLimit 每种触发类型的发起者侧频控（键 push:src:<type>:<uid>）：
	// @提及 / 私聊 / 好友请求各自有频控，防止发起者刷爆派发队列。
	srcLimit  = 30
	srcWindow = time.Minute
	// defaultQueue 派发缓冲。满 = 推送积压，丢弃优于阻塞消息写路径（尽力而为）。
	defaultQueue = 256
	// offlineEdgeTTL 无实时在线查询时的离线边沿有效期：hub 心跳判定半开连接
	// 最长 readIdle(90s)，边沿超过该时长视为无信息，按「不推」兜底（宁缺勿扰）。
	offlineEdgeTTL = 90 * time.Second
)

// DeviceStore Broker 依赖的存储窄接口（*store.Store 满足）。
type DeviceStore interface {
	PushDevicesOfUser(ctx context.Context, userID int64) ([]store.PushDevice, error)
	ChannelMembers(ctx context.Context, channelID int64) ([]store.ChannelMember, error)
}

// OnlineChecker 实时在线判定（*signal.Hub 满足）：复用连接表，别在 presence
// KV 过期上做离线判定（那是 90s 级延迟）。
type OnlineChecker interface {
	UserOnline(uid string) bool
}

// RateLimiter 频控窄接口（*auth.RateLimit 满足）。
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}

// BrokerConfig Broker 装配。Pusher 为 nil 或 Noop 时不要构造 Broker
// （见 NewBroker：未配置 = 不启 worker）。
type BrokerConfig struct {
	Pusher Pusher
	Store  DeviceStore
	Online OnlineChecker // nil 时退化为离线边沿兜底判定（仅测试/降级接线）
	Limits RateLimiter   // nil = 不节流（仅测试）
	Log    *log.Logger
	Queue  int // 派发缓冲条数；<=0 用 defaultQueue
}

// task 一次派发单元。kind 是触发类型（各自频控的命名空间）：
// mention（@提及）/ direct（私聊）/ friend_request（好友请求）。
type task struct {
	kind      string
	actor     string   // 触发者 uid（十进制）
	target    string   // 直接接收者（friend_request 用）
	mentions  []string // mention 的接收者集合
	actorNick string
	route     string // 客户端路由提示，入队时按频道作用域算好
	// direct 触发：worker 用 channelID 反查对端成员（注册即发消息，别在
	// 发送路径上多查一次库）。
	channelID int64
	chName    string
}

// Broker 推送派发器：带缓冲 channel + worker，推送判定与 I/O 全部在 worker
// 里做，Notify 系列只入队，绝不阻塞消息写路径。实现 signal 离线挂钩的
// OnUserOffline（离线边沿）。
type Broker struct {
	pusher Pusher
	store  DeviceStore
	online OnlineChecker
	limits RateLimiter
	log    *log.Logger

	ch     chan task
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// offlineAt 离线边沿记录（OnUserOffline 写、worker 读）：无实时查询时
	// 的兜底判据；有实时查询时仅作旁证，不参与判定。
	mu        sync.Mutex
	offlineAt map[string]time.Time
}

// NewBroker 装配并启动 worker。pusher 为 nil 或 Noop、store 为 nil 时返回 nil
// ——「未配置 = 功能关闭且零副作用」（不启 goroutine、不建 channel）。
func NewBroker(cfg BrokerConfig) *Broker {
	if cfg.Pusher == nil || cfg.Store == nil {
		return nil
	}
	if _, isNoop := cfg.Pusher.(Noop); isNoop {
		return nil
	}
	queue := cfg.Queue
	if queue <= 0 {
		queue = defaultQueue
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Broker{
		pusher:    cfg.Pusher,
		store:     cfg.Store,
		online:    cfg.Online,
		limits:    cfg.Limits,
		log:       safeLog(cfg.Log),
		ch:        make(chan task, queue),
		cancel:    cancel,
		offlineAt: make(map[string]time.Time, 16),
	}
	b.wg.Add(1)
	go b.worker(ctx)
	return b
}

// Close 停止 worker 并等待退出。关闭后入队的任务不再被消费（进程即将退出，
// 推送本就是尽力而为）。
func (b *Broker) Close() {
	b.cancel()
	b.wg.Wait()
}

// NotifyMessage 频道消息触发（@提及 / 私聊）。ch 与 msg 由调用方保证此后不再修改；
// 仅入队，不做任何 I/O。
func (b *Broker) NotifyMessage(ctx context.Context, ch *store.Channel, msg *store.Message, senderNick string) {
	if b == nil || ch == nil || msg == nil {
		return
	}
	if ch.Scope == store.ChannelScopeDirect {
		// 私聊：接收者是对端成员，worker 里反查（发送路径零额外查询）。
		b.enqueue(task{kind: "direct", actor: strconv.FormatInt(msg.UserID, 10),
			route: "conversation:" + strconv.FormatInt(ch.ID, 10), channelID: ch.ID})
		return
	}
	if len(msg.Mentions) == 0 {
		return
	}
	targets := make([]string, 0, len(msg.Mentions))
	for _, uid := range msg.Mentions {
		if u := strconv.FormatInt(uid, 10); u != strconv.FormatInt(msg.UserID, 10) {
			targets = append(targets, u)
		}
	}
	if len(targets) == 0 {
		return
	}
	// 服务器频道路由走 channel:<id>，群私聊（group）语义上是会话。
	route := "channel:" + strconv.FormatInt(ch.ID, 10)
	if ch.Scope == store.ChannelScopeGroup {
		route = "conversation:" + strconv.FormatInt(ch.ID, 10)
	}
	b.enqueue(task{kind: "mention", actor: strconv.FormatInt(msg.UserID, 10),
		mentions: targets, actorNick: senderNick, chName: ch.Name, route: route})
}

// NotifyFriendRequest 好友请求触发：通知被请求方。
func (b *Broker) NotifyFriendRequest(ctx context.Context, targetUID int64, fromUID int64, fromNick string) {
	if b == nil {
		return
	}
	target := strconv.FormatInt(targetUID, 10)
	if target == strconv.FormatInt(fromUID, 10) {
		return
	}
	b.enqueue(task{kind: "friend_request", actor: strconv.FormatInt(fromUID, 10),
		target: target, actorNick: fromNick, route: "friends"})
}

// OnUserOffline 离线边沿（signal.Hub.cleanup 在 unregister 核对同一连接实例、
// 判定该用户最后一个连接实例已关闭后回调）。实现必须快速返回——这里只记内存
// 时间戳，不做任何 I/O。
func (b *Broker) OnUserOffline(uid string) {
	if b == nil || uid == "" {
		return
	}
	b.mu.Lock()
	if b.offlineAt == nil {
		b.offlineAt = make(map[string]time.Time, 16)
	}
	b.offlineAt[uid] = time.Now()
	b.mu.Unlock()
}

// enqueue 非阻塞入队：缓冲满记日志丢任务——推送是尽力而为的旁路，
// 决不允许拖住消息写路径。
func (b *Broker) enqueue(t task) {
	select {
	case b.ch <- t:
	default:
		b.log.Printf("[push] 派发队列已满（%d），丢弃 %s 触发", cap(b.ch), t.kind)
	}
}

// worker 单工作协程：逐任务做离线判定、双频控与投递。
func (b *Broker) worker(ctx context.Context) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-b.ch:
			b.process(ctx, t)
		}
	}
}

// process 单个任务的完整判定链：发起者频控 → 接收者解析 → 离线判定 →
// 10 分钟节流 → 设备查询 → 投递。
func (b *Broker) process(ctx context.Context, t task) {
	switch t.kind {
	case "direct":
		target, err := b.directPeer(ctx, t.channelID, t.actor)
		if err != nil || target == "" {
			return
		}
		if b.deliver(ctx, t.kind, t.actor, target,
			Notification{Title: t.actorNick, Body: "发来一条私聊", Route: t.route}) {
			b.clearOfflineEdge(target)
		}
	case "mention":
		for _, target := range t.mentions {
			if b.deliver(ctx, t.kind, t.actor, target,
				Notification{Title: t.actorNick, Body: "在 #" + t.chName + " 提到你", Route: t.route}) {
				b.clearOfflineEdge(target)
			}
		}
	case "friend_request":
		if b.deliver(ctx, t.kind, t.actor, t.target,
			Notification{Title: t.actorNick, Body: "请求加你为好友", Route: t.route}) {
			b.clearOfflineEdge(t.target)
		}
	}
}

// deliver 单接收者的判定与投递。返回是否实际投递（成功入队给 Pusher）。
func (b *Broker) deliver(ctx context.Context, kind, actor, target string, n Notification) bool {
	if target == actor {
		return false
	}
	// 发起者侧频控（每种触发类型独立键）。
	if b.limits != nil {
		ok, err := b.limits.Allow(ctx, "push:src:"+kind+":"+actor, srcLimit, srcWindow)
		if err != nil {
			b.log.Printf("[push] 发起者频控查询失败（%s → %s），跳过: %v", actor, target, err)
			return false
		}
		if !ok {
			return false
		}
	}
	// 全设备离线才推：连接表实时判定为主，离线边沿为兜底（见 userOffline）。
	if !b.userOffline(target) {
		return false
	}
	// 设备查询先于节流：节流的是「推送」而非「事件」——无设备用户的事件
	// 不烧节流配额（否则先到的无设备事件会吞掉 10 分钟内后续可推送事件）。
	devices, err := b.store.PushDevicesOfUser(ctx, parseUID(target))
	if err != nil {
		b.log.Printf("[push] 设备查询失败（%s）: %v", target, err)
		return false
	}
	if len(devices) == 0 {
		return false
	}
	// 同一接收者 10 分钟最多 1 条（节流键 push:<uid>）；KV 故障 fail-closed，
	// 宁可不推也不刷屏。
	if b.limits != nil {
		ok, err := b.limits.Allow(ctx, pushThrottleKey+target, pushThrottleLimit, pushThrottleTTL)
		if err != nil {
			b.log.Printf("[push] 节流查询失败（%s），跳过: %v", target, err)
			return false
		}
		if !ok {
			return false
		}
	}
	list := make([]Device, 0, len(devices))
	for _, d := range devices {
		list = append(list, Device{Platform: d.Platform, Token: d.Token})
	}
	if err := b.pusher.Push(ctx, list, n); err != nil {
		b.log.Printf("[push] 投递失败（%s，%d 台设备，kind=%s）: %v", target, len(list), kind, err)
		return false
	}
	return true
}

// userOffline 该用户当前是否「全部设备离线」：
//   - 有实时在线检查（生产接线）：以连接表为准——任一连接在线即不推；
//   - 无实时检查（降级接线）：以离线边沿为准，offlineEdgeTTL 内视为离线。
func (b *Broker) userOffline(target string) bool {
	if b.online != nil {
		return !b.online.UserOnline(target)
	}
	b.mu.Lock()
	at, ok := b.offlineAt[target]
	b.mu.Unlock()
	return ok && time.Since(at) < offlineEdgeTTL
}

// clearOfflineEdge 投递成功后清边沿：同一段离线窗口不重复作为兜底判据，
// 下一次判定重新依赖实时查询或新边沿。
func (b *Broker) clearOfflineEdge(target string) {
	b.mu.Lock()
	delete(b.offlineAt, target)
	b.mu.Unlock()
}

// directPeer 私聊对端成员：恰两成员的 direct 频道里不是 actor 的那个。
func (b *Broker) directPeer(ctx context.Context, channelID int64, actorUID string) (string, error) {
	members, err := b.store.ChannelMembers(ctx, channelID)
	if err != nil {
		b.log.Printf("[push] 私聊成员查询失败（频道 %d）: %v", channelID, err)
		return "", err
	}
	for _, m := range members {
		if uid := strconv.FormatInt(m.UserID, 10); uid != actorUID {
			return uid, nil
		}
	}
	return "", nil
}

// parseUID 接收者 uid 字符串 → int64（存层口径）。任务内的 uid 都来自
// store ID 的 FormatInt，解析失败返回 0 即查无设备。
func parseUID(uid string) int64 {
	n, _ := strconv.ParseInt(uid, 10, 64)
	return n
}
