package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/internal/audio"
	"github.com/OpenALC/LiteTalk/client/internal/signal"
	"github.com/coder/websocket"
)

// seqStub 按序下发若干信封后挂起的 WS 桩（比 newDownlinkStubData 多包）。
type seqStub struct {
	*httptest.Server
}

func newSeqStub(t *testing.T, envs ...signal.Envelope) *seqStub {
	t.Helper()
	s := &seqStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, env := range envs {
			raw, err := json.Marshal(env)
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
				return
			}
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *seqStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

// TestVoiceSpeakingForwards voice.speaking（#33）：他人翻转走 StateTable →
// OnPeerSpeaking；自己的翻转直接回调且 mute/deafen 取本地状态（StateTable 无
// self 条目，表值回填会冲掉前端闭麦显示）。
func TestVoiceSpeakingForwards(t *testing.T) {
	type ev struct {
		id       string
		speaking bool
		mute     bool
	}
	got := make(chan ev, 4)
	stub := newSeqStub(t,
		signal.Envelope{Type: signal.TypeHello, TS: 1, Data: json.RawMessage(`{"user":{"id":"42"}}`)},
		signal.Envelope{Type: signal.TypeVoiceSpeaking, TS: 2,
			Data: json.RawMessage(`{"channel_id":"7","user_id":"77","speaking":true}`)},
		signal.Envelope{Type: signal.TypeVoiceSpeaking, TS: 3,
			Data: json.RawMessage(`{"channel_id":"7","user_id":"42","speaking":true}`)},
	)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnPeerSpeaking: func(userID string, speaking, mute, deafen bool) {
			got <- ev{userID, speaking, mute}
		},
	}, nil)
	// 生产里 states 随媒体管线创建（Connect）；这里手工接上同一套转发。
	s.states = audio.NewStateTable()
	s.states.OnChange = func(id string, st audio.PeerVoiceState) {
		got <- ev{id, st.Speaking, st.Mute}
	}
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.muted.Store(true) // 自己已闭麦：voice.speaking 不得把 mute 冲成 false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	for i := 0; i < 2; i++ {
		select {
		case d := <-got:
			wantID := "77"
			wantMute := false
			if i == 1 {
				wantID = "42"
				wantMute = true
			}
			if d.id != wantID || !d.speaking || d.mute != wantMute {
				t.Fatalf("第 %d 个事件 = %+v, want id=%s speaking=true mute=%v", i, d, wantID, wantMute)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 个 OnPeerSpeaking 未到达", i)
		}
	}
}

// TestVoiceSpeakingSelfKeepsLocalState 自己的 voice.speaking 只改 speaking，
// 成员表同步更新（UI 回填用）。
func TestVoiceSpeakingSelfKeepsLocalState(t *testing.T) {
	got := make(chan bool, 2)
	stub := newSeqStub(t,
		signal.Envelope{Type: signal.TypeHello, TS: 1, Data: json.RawMessage(`{"user":{"id":"42"}}`)},
		signal.Envelope{Type: signal.TypeVoiceSpeaking, TS: 2,
			Data: json.RawMessage(`{"channel_id":"7","user_id":"42","speaking":false}`)},
	)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnPeerSpeaking: func(userID string, speaking, mute, deafen bool) {
			got <- speaking
		},
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case sp := <-got:
		if sp {
			t.Fatal("speaking = true, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnPeerSpeaking")
	}
}
