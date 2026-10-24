package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/signal"
)

// TestConversationChangedForwards conversation.changed（#38 B3）解码后把会话 ID
// 与动作原样转给 OnConversationChanged；缺 conversation_id 的坏包不回调。
func TestConversationChangedForwards(t *testing.T) {
	got := make(chan [2]string, 1)
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeConversationChanged, TS: 1,
		Data: json.RawMessage(`{"conversation_id":"9001","action":"member_added"}`),
	})
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnConversationChanged: func(conversationID, action string) {
			got <- [2]string{conversationID, action}
		},
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case d := <-got:
		if d[0] != "9001" || d[1] != "member_added" {
			t.Fatalf("回调参数错误: %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnConversationChanged")
	}
}
