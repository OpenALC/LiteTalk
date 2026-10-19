package voice

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/api"
)

// syncStub 聚合快照测试桩：记录请求路径，按 handler 返回固定响应。
type syncStub struct {
	mu    sync.Mutex
	paths []string
}

func (s *syncStub) record(r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()
}

func (s *syncStub) seen(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (s *syncStub) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// TestFullResyncUsesSyncSnapshot #78：fullResync 优先走 GET /sync——好友在线态
// 从快照回灌（pending 不算在线态），消息仍按 seenChans 逐频道补拉，最后触发
// OnResync；Sync 成功时不得再单拉 /friends。
func TestFullResyncUsesSyncSnapshot(t *testing.T) {
	stub := &syncStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"servers": [],
			"friends": [
				{"id":"1","status":"accepted","incoming":false,"remark":"","group":"",
				 "user":{"id":"99","nickname":"好友A","tag":"0001","email_verified":true,
				         "is_guest":false,"status":1,"online":true,
				         "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}},
				{"id":"2","status":"pending","incoming":true,"remark":"","group":"",
				 "user":{"id":"100","nickname":"待处理","tag":"0002","email_verified":true,
				         "is_guest":false,"status":0,"online":false,
				         "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}
			]
		}`)
	})
	mux.HandleFunc("/api/v1/channels/123/messages", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"500","channel_id":"123","user_id":"99","content":"补拉消息",
			"created_at":"2026-01-01T00:00:00Z","sender_nickname":"好友A","sender_tag":"0001"}]`)
	})
	mux.HandleFunc("/api/v1/friends", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		t.Errorf("Sync 成功时不应回落 ListFriends")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/v1/servers", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		t.Errorf("fullResync 不应自行拉服务器列表（UI 收到 data:resync 后自取）")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var presence []string
	var chats []string
	resyncs := 0
	s := NewSession(api.NewAPIClient(srv.URL), "tok", "n", Events{
		OnPresenceUpdate: func(userID string, online bool) {
			presence = append(presence, fmt.Sprintf("%s:%v", userID, online))
		},
		OnResync: func() { resyncs++ },
	}, nil)
	s.mu.Lock()
	s.seenChans["123"] = true
	s.chatFn = func(channelID, msgID, userID, nickname, content, ts, _ string) {
		chats = append(chats, fmt.Sprintf("%s/%s/%s", channelID, msgID, content))
	}
	s.mu.Unlock()

	s.fullResync()

	if got := stub.seen("/api/v1/sync"); got != 1 {
		t.Fatalf("应调用一次 /sync，实际 %d（路径 %v）", got, stub.snapshot())
	}
	if got := stub.seen("/api/v1/friends"); got != 0 {
		t.Fatalf("Sync 成功时不应调用 /friends，实际 %d", got)
	}
	if got := stub.seen("/api/v1/channels/123/messages"); got != 1 {
		t.Fatalf("应按 seenChans 补拉频道消息，实际 %d", got)
	}
	if len(presence) != 1 || presence[0] != "99:true" {
		t.Fatalf("好友在线态应只回灌 accepted 条目，实际 %v", presence)
	}
	if len(chats) != 1 || chats[0] != "123/500/补拉消息" {
		t.Fatalf("消息回灌不正确: %v", chats)
	}
	if resyncs != 1 {
		t.Fatalf("OnResync 应触发一次，实际 %d", resyncs)
	}
}

// TestFullResyncFallsBackWhenSyncFails #78：聚合端点故障不能拖垮续传补拉——
// 好友回落 ListFriends，消息与 OnResync 照常执行。
func TestFullResyncFallsBackWhenSyncFails(t *testing.T) {
	stub := &syncStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"code":"internal","detail":"boom"}`)
	})
	mux.HandleFunc("/api/v1/friends", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"1","status":"accepted","incoming":false,"remark":"","group":"",
			"user":{"id":"99","nickname":"好友A","tag":"0001","email_verified":true,
			        "is_guest":false,"status":1,"online":false,
			        "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}]`)
	})
	mux.HandleFunc("/api/v1/channels/123/messages", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"500","channel_id":"123","user_id":"99","content":"回落消息",
			"created_at":"2026-01-01T00:00:00Z"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var presence []string
	var chats []string
	resyncs := 0
	s := NewSession(api.NewAPIClient(srv.URL), "tok", "n", Events{
		OnPresenceUpdate: func(userID string, online bool) {
			presence = append(presence, fmt.Sprintf("%s:%v", userID, online))
		},
		OnResync: func() { resyncs++ },
	}, nil)
	s.mu.Lock()
	s.seenChans["123"] = true
	s.chatFn = func(channelID, msgID, userID, nickname, content, ts, _ string) {
		chats = append(chats, content)
	}
	s.mu.Unlock()

	s.fullResync()

	if got := stub.seen("/api/v1/sync"); got != 1 {
		t.Fatalf("应尝试聚合端点一次，实际 %d", got)
	}
	if got := stub.seen("/api/v1/friends"); got != 1 {
		t.Fatalf("聚合端点失败后应回落 ListFriends，实际 %d", got)
	}
	if len(presence) != 1 || presence[0] != "99:false" {
		t.Fatalf("回落好友在线态不正确: %v", presence)
	}
	if len(chats) != 1 || chats[0] != "回落消息" {
		t.Fatalf("聚合端点失败不应影响消息补拉: %v", chats)
	}
	if resyncs != 1 {
		t.Fatalf("OnResync 仍应触发一次，实际 %d", resyncs)
	}
}
