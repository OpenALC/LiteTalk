package webhook

// dispatcher_test.go 出站投递链路测试（#52）。目标全部用 httptest 本地桩；
// 退避用注入的 Wait 记录时长，绝不 sleep 真实时间；SSRF 用默认
// linkpreview.CheckDialable（字面量判定，离线可测）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// fixedNow 测试时钟。
var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// allowAll SSRF 校验放行桩：httptest 目标是 127.0.0.1，默认校验会拒绝。
func allowAll(string) error { return nil }

// newTestDispatcher 常用装配：注入时钟与放行校验，其余用默认值。
func newTestDispatcher(t *testing.T, mem *store.Mem, cfg Config) *Dispatcher {
	t.Helper()
	cfg.CheckURL = allowAll
	cfg.Now = func() time.Time { return fixedNow }
	d := New(cfg, mem)
	t.Cleanup(d.Close)
	return d
}

// hookStub httptest 投递桩：记录请求（头与原文）并按脚本回状态码。
type hookStub struct {
	mu      sync.Mutex
	statuss []int
	got     []recordedRequest
	failN   int // 前 failN 次返回 statuss[0]，其后 200；statuss 为空时恒 200
}

type recordedRequest struct {
	event     string
	timestamp string
	signature string
	body      []byte
}

func (s *hookStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		idx := len(s.got)
		var code int
		switch {
		case len(s.statuss) == 0:
			code = http.StatusOK
		case idx < s.failN:
			code = s.statuss[0]
		default:
			code = http.StatusOK
		}
		s.mu.Unlock()
		body, _ := io.ReadAll(r.Body) // 读全量：单次 Read 不保证填满，签名断言会抖
		s.mu.Lock()
		s.got = append(s.got, recordedRequest{
			event:     r.Header.Get("X-LiteTalk-Event"),
			timestamp: r.Header.Get("X-LiteTalk-Timestamp"),
			signature: r.Header.Get("X-LiteTalk-Signature"),
			body:      body,
		})
		s.mu.Unlock()
		w.WriteHeader(code)
	}
}

func (s *hookStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *hookStub) last() recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[len(s.got)-1]
}

func (s *hookStub) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = nil
}

// seedServerChannel 建服务器与文字频道（worker 经频道反查服务器归属）。
func seedServerChannel(t *testing.T, mem *store.Mem) (serverID, channelID int64) {
	t.Helper()
	srv := &store.Server{Name: "srv", OwnerID: 1}
	if err := mem.CreateServer(context.Background(), srv); err != nil {
		t.Fatal(err)
	}
	ch := &store.Channel{ServerID: srv.ID, Name: "general", Type: store.ChannelText, Scope: store.ChannelScopeServer}
	if err := mem.CreateChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	return srv.ID, ch.ID
}

// seedHook 建一个出站订阅。
func seedHook(t *testing.T, mem *store.Mem, serverID int64, url, events string) *store.OutboundWebhook {
	t.Helper()
	w := &store.OutboundWebhook{ServerID: serverID, URL: url, Secret: "secret0123456789abcdef0123456789abcdef", Events: events}
	if err := mem.CreateOutboundWebhook(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w
}

// deliverTask 直呼 process（等价 OnWebhookEvent 出队后的处理，测试确定性强）。
func deliverTask(d *Dispatcher, event string, channelID int64) {
	d.process(context.Background(), outboundTask{
		event:     event,
		channelID: strconv.FormatInt(channelID, 10),
		payload:   json.RawMessage(`{"content":"hi"}`),
	})
}

func TestSignAndVerify(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"event":"message.created"}`)
	ts := fixedNow.Unix()
	sig := Sign(secret, ts, body)

	if !VerifySignature(secret, ts, body, sig, fixedNow) {
		t.Fatal("正确签名应通过")
	}
	if VerifySignature("wrong", ts, body, sig, fixedNow) {
		t.Fatal("错误密钥的签名应拒绝")
	}
	if VerifySignature(secret, ts, []byte(`{"event":"tampered"}`), sig, fixedNow) {
		t.Fatal("body 被篡改应拒绝")
	}
	// 时间戳窗口：±5 分钟内放行，之外拒绝（防重放）。
	if !VerifySignature(secret, fixedNow.Add(5*time.Minute).Unix(), body, Sign(secret, fixedNow.Add(5*time.Minute).Unix(), body), fixedNow) {
		t.Fatal("恰好 5 分钟偏差应放行")
	}
	if VerifySignature(secret, fixedNow.Add(6*time.Minute).Unix(), body, Sign(secret, fixedNow.Add(6*time.Minute).Unix(), body), fixedNow) {
		t.Fatal("超过 5 分钟的时间戳应拒绝（过期）")
	}
	if VerifySignature(secret, fixedNow.Add(-6*time.Minute).Unix(), body, Sign(secret, fixedNow.Add(-6*time.Minute).Unix(), body), fixedNow) {
		t.Fatal("过去时刻超过 5 分钟应拒绝")
	}
}

func TestValidEvents(t *testing.T) {
	if !ValidEvents([]string{EventMessageCreated}) {
		t.Fatal("白名单内单事件应通过")
	}
	if !ValidEvents([]string{EventMessageCreated, EventMemberJoined, EventMemberLeft, EventChannelCreated, EventChannelDeleted}) {
		t.Fatal("五类全订阅应通过")
	}
	for _, bad := range [][]string{
		nil,
		{},
		{"message.created", "message.created"}, // 重复
		{"chat.message"},                       // 白名单外（WS 消息类型不是订阅事件）
		{""},
		{EventMessageCreated, "star.created"},
	} {
		if ValidEvents(bad) {
			t.Fatalf("%v 应拒绝", bad)
		}
	}
	if !ValidEvent(EventMessageCreated) || ValidEvent("chat.message") {
		t.Fatal("ValidEvent 判定不符")
	}
}

// TestDeliverSuccessSigning 正确投递：桩侧复算 HMAC，签名/事件/时间戳头齐全；
// 私聊频道（server_id=0）、dev 房间与未订阅事件不出站。
func TestDeliverSuccessSigning(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	w := seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{})

	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != 1 {
		t.Fatalf("应投递 1 次，实际 %d", stub.count())
	}
	req := stub.last()
	if req.event != EventMessageCreated {
		t.Fatalf("X-LiteTalk-Event 应为 %s，实际 %s", EventMessageCreated, req.event)
	}
	if req.timestamp != strconv.FormatInt(fixedNow.Unix(), 10) {
		t.Fatalf("X-LiteTalk-Timestamp 应取注入时钟，实际 %s", req.timestamp)
	}
	ts, err := strconv.ParseInt(req.timestamp, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := "sha256=" + Sign(w.Secret, ts, req.body)
	if req.signature != want {
		t.Fatalf("签名不符：want %s got %s", want, req.signature)
	}
	// 投递体形状：event/server_id/data；data 是原网关载荷。
	var env outboundEnvelope
	if err := json.Unmarshal(req.body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Event != EventMessageCreated || env.ServerID != strconv.FormatInt(serverID, 10) {
		t.Fatalf("投递体不符: %+v", env)
	}
	if string(env.Data) != `{"content":"hi"}` {
		t.Fatalf("data 应原样携带网关载荷: %s", env.Data)
	}
	dl, err := mem.WebhookDeliveriesBefore(context.Background(), time.Now().Add(time.Hour), 10)
	if err != nil || len(dl) != 1 || !dl[0].OK || dl[0].StatusCode != http.StatusOK {
		t.Fatalf("应落一条成功投递记录: %v %v", dl, err)
	}

	// 未订阅的事件与无服务器归属的频道都不出站。
	stub.reset()
	deliverTask(d, EventMemberJoined, channelID)
	if stub.count() != 0 {
		t.Fatal("未订阅事件不应出站")
	}
	d.process(context.Background(), outboundTask{event: EventMessageCreated, channelID: "dev-room", payload: json.RawMessage(`{}`)})
	if stub.count() != 0 {
		t.Fatal("非数字频道（dev 内置房间）不应出站")
	}
}

// TestDeliverBackoffCurve 退避曲线：恒 500，base=1s、RetryMax=4 →
// 重试前等待 1s→2s→4s→8s，总尝试 5 次（RetryMax+1），最终落失败记录。
func TestDeliverBackoffCurve(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{statuss: []int{http.StatusInternalServerError}, failN: 1 << 30}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	seedHook(t, mem, serverID, hs.URL, EventMessageCreated)

	var waits []time.Duration
	d := newTestDispatcher(t, mem, Config{
		RetryMax:    4,
		BackoffBase: time.Second,
		Wait: func(ctx context.Context, dur time.Duration) error {
			waits = append(waits, dur)
			return nil
		},
	})
	deliverTask(d, EventMessageCreated, channelID)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("退避次数应为 %d，实际 %v", len(want), waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("第 %d 次退避应为 %s，实际 %s", i, want[i], waits[i])
		}
	}
	if stub.count() != 5 {
		t.Fatalf("总尝试应为 5 次，实际 %d", stub.count())
	}
	dl, _ := mem.WebhookDeliveriesBefore(context.Background(), time.Now().Add(time.Hour), 10)
	if len(dl) != 1 || dl[0].OK || dl[0].Attempts != 5 || dl[0].StatusCode != 500 {
		t.Fatalf("终态记录应为失败 5 次尝试: %+v", dl)
	}
}

// TestDeliverBackoffCap 封顶 5 分钟：base=4m → 4m、5m、5m。
func TestDeliverBackoffCap(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{statuss: []int{http.StatusInternalServerError}, failN: 1 << 30}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	var waits []time.Duration
	d := newTestDispatcher(t, mem, Config{
		RetryMax:    3,
		BackoffBase: 4 * time.Minute,
		Wait: func(ctx context.Context, dur time.Duration) error {
			waits = append(waits, dur)
			return nil
		},
	})
	deliverTask(d, EventMessageCreated, channelID)
	want := []time.Duration{4 * time.Minute, MaxBackoff, MaxBackoff}
	for i, w := range want {
		if i >= len(waits) || waits[i] != w {
			t.Fatalf("退避应为 %v，实际 %v", want, waits)
		}
	}
}

// TestDeliver4xxNoRetry 除 429 外的 4xx 立即放弃：400 只打一次、零退避。
func TestDeliver4xxNoRetry(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{statuss: []int{http.StatusBadRequest}, failN: 1 << 30}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	waits := 0
	d := newTestDispatcher(t, mem, Config{
		RetryMax: 8,
		Wait: func(ctx context.Context, dur time.Duration) error {
			waits++
			return nil
		},
	})
	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != 1 || waits != 0 {
		t.Fatalf("400 不应重试：请求 %d 次、退避 %d 次", stub.count(), waits)
	}
}

// TestDeliver429Retries 429 视为可重试。
func TestDeliver429Retries(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{statuss: []int{http.StatusTooManyRequests}, failN: 1}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	var waits []time.Duration
	d := newTestDispatcher(t, mem, Config{
		RetryMax:    8,
		BackoffBase: time.Second,
		Wait: func(ctx context.Context, dur time.Duration) error {
			waits = append(waits, dur)
			return nil
		},
	})
	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != 2 || len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("429 应重试一次（退避 1s）：请求 %d 次，退避 %v", stub.count(), waits)
	}
}

// TestDeliverSuccessAfterRetries 第 3 次尝试成功：前两次 500，其后 200。
func TestDeliverSuccessAfterRetries(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{statuss: []int{http.StatusInternalServerError}, failN: 2}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{
		RetryMax:    8,
		BackoffBase: time.Second,
		Wait:        func(ctx context.Context, _ time.Duration) error { return nil },
	})
	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != 3 {
		t.Fatalf("应尝试 3 次，实际 %d", stub.count())
	}
	dl, _ := mem.WebhookDeliveriesBefore(context.Background(), time.Now().Add(time.Hour), 10)
	if len(dl) != 1 || !dl[0].OK || dl[0].Attempts != 3 {
		t.Fatalf("终态应为成功 3 次尝试: %+v", dl)
	}
}

// TestSSRFBlockedAtDelivery 投递前静态 SSRF 复检（默认 linkpreview.CheckDialable）：
// 内网字面量直接拦下，不发起任何请求。
func TestSSRFBlockedAtDelivery(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	seedHook(t, mem, serverID, "http://10.0.0.1/hook", EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{})
	// 覆盖回默认校验（newTestDispatcher 已放行）。
	d.cfg.CheckURL = nil
	d.cfg.fill()
	deliverTask(d, EventMessageCreated, channelID)
	dl, _ := mem.WebhookDeliveriesBefore(context.Background(), time.Now().Add(time.Hour), 10)
	if len(dl) != 1 || dl[0].OK || dl[0].Error != "ssrf_blocked" {
		t.Fatalf("内网目标应被拦并落 ssrf_blocked 记录: %+v", dl)
	}
}

// TestSafeDialRejectsBlockedIP 拨号层二次校验：解析结果落在内网段即拒绝
// （字面量 IP 不触发真实 DNS）。
func TestSafeDialRejectsBlockedIP(t *testing.T) {
	mem := store.NewMem()
	d := newTestDispatcher(t, mem, Config{})
	d.cfg.CheckURL = nil
	d.cfg.fill()
	if _, err := d.safeDial(context.Background(), "tcp", "10.0.0.1:80"); err == nil {
		t.Fatal("拨号到 10.0.0.1 应被拒")
	}
	if _, err := d.safeDial(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("拨号到云元数据地址应被拒")
	}
}

// TestBreakerAfterConsecutiveFailures 连续失败 20 次熔断：第 20 条失败后订阅被
// 停用并写 disabled_reason，之后的事件不再出站；恢复投递与计数清零一起验证。
func TestBreakerAfterConsecutiveFailures(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{statuss: []int{http.StatusInternalServerError}, failN: 1 << 30}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	w := seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{RetryMax: 0}) // 每事件一次尝试

	for i := 0; i < DefaultBreakerAt; i++ {
		deliverTask(d, EventMessageCreated, channelID)
	}
	got, err := mem.OutboundWebhookByID(context.Background(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled || got.DisabledReason != DisabledReason {
		t.Fatalf("连续失败 %d 次后应自动停用: %+v", DefaultBreakerAt, got)
	}
	before := stub.count()
	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != before {
		t.Fatal("熔断停用后不应再出站")
	}
	// 手动重新启用清零计数（API 层语义，Store 侧行为在此验证），并让桩从
	// 下一次请求起返回 200：成功应把连败计数清零。
	got.Disabled, got.DisabledReason, got.FailCount = false, "", 0
	if err := mem.UpdateOutboundWebhook(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.failN = DefaultBreakerAt // 前 20 次已失败，此后成功
	stub.mu.Unlock()
	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != before+1 {
		t.Fatal("重新启用后应恢复投递")
	}
	dl, _ := mem.WebhookDeliveriesBefore(context.Background(), time.Now().Add(time.Hour), 500)
	fails := 0
	for _, x := range dl {
		if !x.OK {
			fails++
		}
	}
	if fails != DefaultBreakerAt {
		t.Fatalf("失败记录应为 %d 条，实际 %d", DefaultBreakerAt, fails)
	}
	hook, _ := mem.OutboundWebhookByID(context.Background(), w.ID)
	if hook.FailCount != 0 {
		t.Fatalf("成功后连败计数应清零，实际 %d", hook.FailCount)
	}
}

// sweepStore 只服务清理用例的存储桩：返回预置记录、记录删除调用。
type sweepStore struct {
	deliveries []store.WebhookDelivery
	deleted    [][]int64
}

func (s *sweepStore) ChannelByID(context.Context, int64) (*store.Channel, error) {
	return nil, store.ErrNotFound
}
func (s *sweepStore) OutboundWebhooksOfServer(context.Context, int64) ([]store.OutboundWebhook, error) {
	return nil, nil
}
func (s *sweepStore) RecordWebhookDelivery(context.Context, *store.WebhookDelivery, int, string) error {
	return nil
}
func (s *sweepStore) WebhookDeliveriesBefore(_ context.Context, before time.Time, limit int) ([]store.WebhookDelivery, error) {
	out := []store.WebhookDelivery{}
	for _, d := range s.deliveries {
		if d.CreatedAt.Before(before) {
			out = append(out, d)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *sweepStore) DeleteWebhookDeliveries(_ context.Context, ids []int64) error {
	s.deleted = append(s.deleted, ids)
	return nil
}

// TestSweepDeletesExpiredDeliveryRecords 投递记录清理：只删早于保留期的，
// 新记录保留。
func TestSweepDeletesExpiredDeliveryRecords(t *testing.T) {
	old := fixedNow.Add(-8 * 24 * time.Hour)
	fresh := fixedNow.Add(-time.Hour)
	st := &sweepStore{deliveries: []store.WebhookDelivery{
		{ID: 1, WebhookID: 7, Event: EventMessageCreated, CreatedAt: old},
		{ID: 2, WebhookID: 7, Event: EventMessageCreated, CreatedAt: old},
		{ID: 3, WebhookID: 7, Event: EventMemberJoined, CreatedAt: fresh},
	}}
	d := New(Config{Now: func() time.Time { return fixedNow }}, st)
	d.sweepOnce(context.Background())
	if len(st.deleted) != 1 || len(st.deleted[0]) != 2 {
		t.Fatalf("应删掉两条过期记录: %v", st.deleted)
	}
	if st.deleted[0][0] != 1 || st.deleted[0][1] != 2 {
		t.Fatalf("删错的 id: %v", st.deleted[0])
	}
}

// TestOutletDropsWhenNotStarted 未启动（零订阅部署）时事件出口直接丢弃。
func TestOutletDropsWhenNotStarted(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	seedHook(t, mem, serverID, "http://10.0.0.1/hook", EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{})
	d.started.Store(false) // 显式回到未启动态
	d.OnWebhookEvent(EventMessageCreated, strconv.FormatInt(channelID, 10), json.RawMessage(`{}`))
	if d.started.Load() {
		t.Fatal("出口不应启动 worker")
	}
}

// TestResetSecretTakesEffectImmediately reset_secret 后旧 secret 立即失效：
// 投递器逐次读行，下一次投递即用新 secret 签名（持有旧 secret 的接收方验签必失败）。
func TestResetSecretTakesEffectImmediately(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	w := seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{})
	oldSecret := w.Secret

	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != 1 {
		t.Fatal("第一次投递应到达")
	}
	// 模拟 PATCH reset_secret：换新 secret（API 层经 UpdateOutboundWebhook 落库）。
	newSecret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	w.Secret = newSecret
	if err := mem.UpdateOutboundWebhook(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	deliverTask(d, EventMessageCreated, channelID)
	if stub.count() != 2 {
		t.Fatal("reset 后投递应到达")
	}
	// 新投递的签名必须用新 secret 校验通过、旧 secret 校验失败。
	req := stub.last()
	ts, _ := strconv.ParseInt(req.timestamp, 10, 64)
	if req.signature != "sha256="+Sign(newSecret, ts, req.body) {
		t.Fatal("reset 后应使用新 secret 签名")
	}
	if VerifySignature(oldSecret, ts, req.body, strings.TrimPrefix(req.signature, "sha256="), fixedNow) {
		t.Fatal("旧 secret 校验应失败（已失效）")
	}
}

// TestOutletEndToEnd 事件出口全链路：Start 后 OnWebhookEvent 异步投递到订阅目标。
func TestOutletEndToEnd(t *testing.T) {
	mem := store.NewMem()
	serverID, channelID := seedServerChannel(t, mem)
	stub := &hookStub{}
	hs := httptest.NewServer(stub.handler())
	defer hs.Close()
	seedHook(t, mem, serverID, hs.URL, EventMessageCreated)
	d := newTestDispatcher(t, mem, Config{Workers: 2})
	d.Start(nil)
	d.OnWebhookEvent(EventMessageCreated, fmt.Sprintf("%d", channelID), json.RawMessage(`{"content":"e2e"}`))
	deadline := time.Now().Add(2 * time.Second)
	for stub.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("等待异步投递超时")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if stub.last().event != EventMessageCreated {
		t.Fatal("异步投递事件类型不符")
	}
}
