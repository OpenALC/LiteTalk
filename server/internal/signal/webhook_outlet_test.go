package signal

// webhook_outlet_test.go 出站 webhook 事件出口接线（#52）：chat.message 广播必须
// 以 message.created 事件名与原载荷投给出口，且出口收不到其他事件类型。

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/webhook"
)

// recordingOutlet 事件出口桩：记录收到的 (事件名, 频道, 载荷)。
type recordingOutlet struct {
	mu   sync.Mutex
	got  []string
	raws []json.RawMessage
}

func (o *recordingOutlet) OnWebhookEvent(eventType, channelID string, payload json.RawMessage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, eventType+"\x00"+channelID)
	o.raws = append(o.raws, payload)
}

func (o *recordingOutlet) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.got)
}

// TestChatBroadcastFeedsWebhookOutlet 出口只在 message.created（chat.message）上
// 触发：事件名与频道上下文正确、载荷是原网关 JSON；presence 等其他广播不进出口。
func TestChatBroadcastFeedsWebhookOutlet(t *testing.T) {
	f := newChatFixture(t)
	outlet := &recordingOutlet{}
	f.hub.SetWebhookOutlet(outlet)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "给 webhook 的消息"})
	a.recvUntil(TypeChatMessage)

	if outlet.count() != 1 {
		t.Fatalf("出口应收 1 条事件，实际 %d", outlet.count())
	}
	if outlet.got[0] != webhook.EventMessageCreated+"\x00"+chID {
		t.Fatalf("事件名/频道不符: %q", outlet.got[0])
	}
	var msg ChatMessageData
	if err := json.Unmarshal(outlet.raws[0], &msg); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v", err)
	}
	if msg.Content != "给 webhook 的消息" || msg.ChannelID != chID {
		t.Fatalf("载荷与网关广播不一致: %+v", msg)
	}

	// 非消息类广播（unread 计数）不得进出口。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "第二条"})
	a.recvUntil(TypeChatMessage)
	a.recvUntil(TypeChannelUnread)
	if outlet.count() != 2 {
		t.Fatalf("第二条消息应再触发一次出口，实际 %d", outlet.count())
	}
}
