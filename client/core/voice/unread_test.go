package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/signal"
)

// TestChannelUnreadForwards channel.unread（#40）解码后把频道与计数原样转给
// OnChannelUnread；缺 channel_id 的坏包不回调。
func TestChannelUnreadForwards(t *testing.T) {
	t.Run("正常事件", func(t *testing.T) {
		got := make(chan signal.ChannelUnreadData, 1)
		stub, _ := newDownlinkStubData(t, signal.Envelope{
			Type: signal.TypeChannelUnread, TS: 1,
			Data: json.RawMessage(`{"channel_id":"11","unread_count":3,"mention_count":1}`),
		})
		s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
			OnChannelUnread: func(d signal.ChannelUnreadData) { got <- d },
		}, nil)
		s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
		defer s.ws.Close()
		s.registerHandlers()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.ws.Run(ctx)

		select {
		case d := <-got:
			if d.ChannelID != "11" || d.UnreadCount != 3 || d.MentionCount != 1 {
				t.Fatalf("回调参数错误: %+v", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("未收到 OnChannelUnread")
		}
	})

	t.Run("缺 channel_id 不回调", func(t *testing.T) {
		got := make(chan signal.ChannelUnreadData, 1)
		stub, _ := newDownlinkStubData(t, signal.Envelope{
			Type: signal.TypeChannelUnread, TS: 1,
			Data: json.RawMessage(`{"unread_count":3,"mention_count":1}`),
		})
		s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
			OnChannelUnread: func(d signal.ChannelUnreadData) { got <- d },
		}, nil)
		s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
		defer s.ws.Close()
		s.registerHandlers()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.ws.Run(ctx)

		select {
		case d := <-got:
			t.Fatalf("坏包不应回调: %+v", d)
		case <-time.After(300 * time.Millisecond):
		}
	})
}
