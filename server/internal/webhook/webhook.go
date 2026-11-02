// Package webhook 出站投递与入站投递入口的公共逻辑（#52）。
//
// 出站：服务器级事件订阅（五个白名单事件，见 Event* 常量），事件出口在
// signal.Hub.broadcastEvent（读快照后异步入队），worker 池做 HMAC-SHA256 签名
// 投递、指数退避重试、4xx 短路与连续失败熔断，最终态落 webhook_deliveries 并
// 按保留期清理。入站：/api/v1/webhooks/{id}/{secret} 的 secret 校验与消息落库
// 依赖的密钥生成也在这里（REST 处理器在 internal/api）。
//
// 签名口径（接收方照此校验，openapi 同文）：
//  1. 取 X-LiteTalk-Timestamp（unix 秒），与本地时刻相差超过 ±5 分钟即拒绝（防重放）；
//  2. 以订阅 secret 为密钥，对 "<timestamp>.<原始请求体>" 计算 HMAC-SHA256；
//  3. 与 X-LiteTalk-Signature 头的 "sha256=<hex>" 部分做常数时间比对。
//
// 不给投递加 Bearer：对外单向通知，签名已足够。
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 事件白名单（批次 6 契约）：订阅时校验，事件出口只挂这五类，新增需改契约。
// 事件源：message.created 在 hub 事件流（signal 包 broadcastEvent 尾部的出口）；
// member.joined / channel.created / channel.deleted 由 REST 成功点（事务提交后）
// 经 api.Server.notifyWebhookEvent 派发；member.left 触发源暂缺（见常量注释）。
const (
	EventMessageCreated = "message.created"
	EventMemberJoined   = "member.joined"
	// EventMemberLeft 目前休眠：store.RemoveMember 没有 REST 调用方（无删除成员
	// 入口）。有了之后在对应成功点经 notifyWebhookEvent 派发一行即点亮，投递
	// 链路（订阅校验/签名/重试/熔断）已就绪。
	EventMemberLeft     = "member.left"
	EventChannelCreated = "channel.created"
	EventChannelDeleted = "channel.deleted"
)

var validEvents = map[string]bool{
	EventMessageCreated: true,
	EventMemberJoined:   true,
	EventMemberLeft:     true,
	EventChannelCreated: true,
	EventChannelDeleted: true,
}

// ValidEvent 单个事件名是否在白名单内。
func ValidEvent(event string) bool { return validEvents[event] }

// ValidEvents 订阅集合校验：非空、不重复、全部在白名单内。
func ValidEvents(events []string) bool {
	if len(events) == 0 {
		return false
	}
	seen := make(map[string]bool, len(events))
	for _, e := range events {
		if !validEvents[e] || seen[e] {
			return false
		}
		seen[e] = true
	}
	return true
}

// subscribed 出站订阅的 Events（空格分隔）是否包含事件。
func subscribed(events, event string) bool {
	for _, f := range strings.Fields(events) {
		if f == event {
			return true
		}
	}
	return false
}

// 熔断、退避与清理参数。全部有安全默认（批次 6 §0.8）；RETRY_MAX/BACKOFF_BASE/
// TIMEOUT/WORKERS 可经 LITETALK_WEBHOOK_* 调整（解析在 cmd/server）。
const (
	// DefaultRetryMax 最多重试 8 次（不含首发，总尝试 = RetryMax+1）。
	DefaultRetryMax = 8
	// DefaultBackoffBase 退避基数：第 k 次重试前等待 base*2^(k-1)。
	DefaultBackoffBase = time.Second
	// MaxBackoff 单次退避封顶 5 分钟。
	MaxBackoff = 5 * time.Minute
	// DefaultTimeout 单次投递整体超时（连接+请求+响应）。
	DefaultTimeout = 10 * time.Second
	// DefaultWorkers worker 池默认大小。
	DefaultWorkers = 4
	// DefaultQueue 派发缓冲条数；满即丢弃（出站通知是尽力而为的旁路）。
	DefaultQueue = 256
	// DefaultBreakerAt 连续失败达到该次数自动停用订阅。
	DefaultBreakerAt = 20
	// DisabledReason 熔断停用写入 disabled_reason 的值（列表可见，手工启用清零）。
	DisabledReason = "consecutive_failures"
	// DefaultRetention 投递记录保留期。
	DefaultRetention = 7 * 24 * time.Hour
	// sweepEvery 清理 goroutine 的扫描间隔。
	sweepEvery = time.Hour
	// maxRedirects 最多跟随的跳转次数（第 4 个 Location 拒绝）。
	maxRedirects = 3
	// deliveryErrorMax 投递记录 error 摘要的字符上限（VARCHAR(255)）。
	deliveryErrorMax = 255
)

// SignatureTolerance 接收方应接受的时间戳偏差（防重放窗口，openapi 同文）。
const SignatureTolerance = 5 * time.Minute

// Sign 计算出站签名：HMAC-SHA256(secret, "<timestamp>.<body>") 的十六进制串。
// 调用方把返回值加 "sha256=" 前缀放进 X-LiteTalk-Signature。导出仅供测试与
// 接收方参考实现；服务端自身不校验入站签名（出站单向通知）。
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature 接收方的参考校验实现：时间戳在 ±SignatureTolerance 内且签名
// 常数时间匹配。导出供接收方测试与文档示例复用。
func VerifySignature(secret string, timestamp int64, body []byte, signatureHex string, now time.Time) bool {
	delta := now.Sub(time.Unix(timestamp, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > SignatureTolerance {
		return false
	}
	want := Sign(secret, timestamp, body)
	return subtle.ConstantTimeCompare([]byte(want), []byte(signatureHex)) == 1
}

// NewSecret 生成出站/入站共用的投递密钥（32 字节随机 → 64 位小写 hex）。
func NewSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 webhook secret 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// outboundEnvelope 出站投递体。ID 一律十进制字符串（批次 6 §0.4）；data 是原网关
// 事件载荷（message.created 即 chat.message）。时间戳只在头里——签名对象是
// "<ts>.<本 JSON 原文>"，体里重复放时间戳没有校验价值。
type outboundEnvelope struct {
	Event     string          `json:"event"`
	ServerID  string          `json:"server_id"`
	ChannelID string          `json:"channel_id,omitempty"`
	Data      json.RawMessage `json:"data"`
}
