package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #92 好友频道 presence 快照（/friends 与 /sync 的 FriendEntry.channel_id/server_id）：
// 权限过滤（查看者 ViewChannel）与隐私键缺省——无权限/故障时原始 JSON 不得出现
// channel_id/server_id 键，且不得 500。载荷不含频道名。

// stubVoice 固定返回的语音房反查（VoicePresence）。
type stubVoice struct {
	ch, sid string
	ok      bool
}

func (s stubVoice) VoiceChannelOf(string) (string, string, bool) {
	if !s.ok {
		return "", "", false
	}
	return s.ch, s.sid, true
}

// getRaw 取原始响应体（隐私键断言要看真实 JSON，不能只看解码后的 map）。
func getRaw(t *testing.T, h http.Handler, path, token string, wantStatus int) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(clientVersionHeader, testClientVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s: 期望 %d 实际 %d body=%s", path, wantStatus, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func assertNoChanKeys(t *testing.T, raw []byte) {
	t.Helper()
	for _, k := range []string{"channel_id", "server_id"} {
		if bytes.Contains(raw, []byte(k)) {
			t.Fatalf("响应不得含 %s 键: %s", k, raw)
		}
	}
}

// makeAcceptedFriends 建 A→B 的已接受好友关系，B 置为在线。
func makeAcceptedFriends(t *testing.T, h http.Handler, mem *store.Mem, aTok string, bEmail, bNick string) (string, int64) {
	t.Helper()
	bTok, _ := registerUser(t, h, mem, bEmail, bNick, "password123")
	b, err := mem.UserByEmail(t.Context(), bEmail)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("%s#%s", b.Nickname, b.Tag)}, 201)
	fid := int(asID(fr["id"]))
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	if err := mem.Set(t.Context(), presenceKeyOf(b.ID), "1", 0); err != nil {
		t.Fatalf("Set presence: %v", err)
	}
	return bTok, b.ID
}

// TestFriendsChannelSnapshotVisible 有 View 权限的查看者拿到频道快照（/friends 与 /sync）。
func TestFriendsChannelSnapshotVisible(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aTok, _ := registerUser(t, h, mem, "fcv-a@ex.com", "视见甲", "password123")
	_, bID := makeAcceptedFriends(t, h, mem, aTok, "fcv-b@ex.com", "视见乙")

	// A 建服开语音房（owner 天然有 ViewChannel）。
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "语音服"}, 201)
	sid := asID(sv["id"])
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok,
		map[string]any{"name": "机密开黑房", "type": "voice"}, 201)
	chID := asID(ch["id"])
	srv.voice = stubVoice{ch: strconv.FormatInt(chID, 10), sid: strconv.FormatInt(sid, 10), ok: true}

	raw := getRaw(t, h, "/api/v1/friends", aTok, 200)
	list := getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	entry := list[0].(map[string]any)
	if asID(entry["channel_id"]) != chID || asID(entry["server_id"]) != sid {
		t.Fatalf("应带频道快照 (%d,%d)，实际 %v/%v；body=%s", chID, sid, entry["channel_id"], entry["server_id"], raw)
	}
	// 隐私：快照只出 ID，频道名不得出现在好友投影里。
	if strings.Contains(string(raw), "机密开黑房") {
		t.Fatalf("好友快照不得泄露频道名: %s", raw)
	}

	// /sync 同口径。
	syncBody := getJSON(t, h, "/api/v1/sync", aTok, 200).(map[string]any)
	syncFriends := syncBody["friends"].([]any)
	sentry := syncFriends[0].(map[string]any)
	if asID(sentry["channel_id"]) != chID || asID(sentry["server_id"]) != sid {
		t.Fatalf("/sync 应带频道快照: %v", sentry)
	}
	if asID(entry["user"].(map[string]any)["id"]) != bID {
		t.Fatalf("对端应为 B=%d，实际 %v", bID, entry["user"])
	}
}

// TestFriendsChannelHiddenWithoutView 查看者无 ViewChannel 时键缺省（原始 JSON 不得
// 出现 channel_id/server_id），且不得 500。
func TestFriendsChannelHiddenWithoutView(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aTok, _ := registerUser(t, h, mem, "fch-a@ex.com", "无权甲", "password123")
	bTok, _ := makeAcceptedFriends(t, h, mem, aTok, "fch-b@ex.com", "无权乙")

	// 语音房在 B 自己的服里：A 不是成员 → 无 ViewChannel。
	sv := postJSON(t, h, "/api/v1/servers", bTok, map[string]string{"name": "乙的服"}, 201)
	sid := asID(sv["id"])
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), bTok,
		map[string]any{"name": "乙的房", "type": "voice"}, 201)
	chID := asID(ch["id"])
	srv.voice = stubVoice{ch: strconv.FormatInt(chID, 10), sid: strconv.FormatInt(sid, 10), ok: true}

	for _, path := range []string{"/api/v1/friends", "/api/v1/sync"} {
		raw := getRaw(t, h, path, aTok, 200)
		assertNoChanKeys(t, raw)
	}
}

// TestFriendsChannelPendingAndOfflineHides pending 关系与离线对端不带频道字段。
func TestFriendsChannelPendingAndOfflineHides(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aTok, _ := registerUser(t, h, mem, "fcp-a@ex.com", "待离甲", "password123")
	bTok, _ := registerUser(t, h, mem, "fcp-b@ex.com", "待离乙", "password123")
	b, err := mem.UserByEmail(t.Context(), "fcp-b@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "语音服"}, 201)
	sid := asID(sv["id"])
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok,
		map[string]any{"name": "房", "type": "voice"}, 201)
	chID := asID(ch["id"])
	srv.voice = stubVoice{ch: strconv.FormatInt(chID, 10), sid: strconv.FormatInt(sid, 10), ok: true}

	// pending：B 在线也不带字段。
	if err := mem.Set(t.Context(), presenceKeyOf(b.ID), "1", 0); err != nil {
		t.Fatalf("Set presence: %v", err)
	}
	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("%s#%s", b.Nickname, b.Tag)}, 201)
	fid := int(asID(fr["id"]))
	assertNoChanKeys(t, getRaw(t, h, "/api/v1/friends", aTok, 200))

	// 接受后在线：带字段；删 presence 后离线：键缺省。
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	raw := getRaw(t, h, "/api/v1/friends", aTok, 200)
	if !bytes.Contains(raw, []byte("channel_id")) {
		t.Fatalf("在线 accepted 应带频道快照: %s", raw)
	}
	if err := mem.Del(t.Context(), presenceKeyOf(b.ID)); err != nil {
		t.Fatalf("Del presence: %v", err)
	}
	assertNoChanKeys(t, getRaw(t, h, "/api/v1/friends", aTok, 200))
}

// TestFriendsChannelVoicePresenceDegraded VoicePresence 缺失/失败/反查出不存在的
// 资源：省略字段、不 500（故障不当有权也不当无权）。
func TestFriendsChannelVoicePresenceDegraded(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aTok, _ := registerUser(t, h, mem, "fcd-a@ex.com", "降级甲", "password123")
	makeAcceptedFriends(t, h, mem, aTok, "fcd-b@ex.com", "降级乙")
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "语音服"}, 201)
	sid := asID(sv["id"])
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok,
		map[string]any{"name": "房", "type": "voice"}, 201)
	chID := asID(ch["id"])

	// nil：省略不 500。
	srv.voice = nil
	assertNoChanKeys(t, getRaw(t, h, "/api/v1/friends", aTok, 200))

	// ok=false（反查失败）：省略不 500。
	srv.voice = stubVoice{ok: false}
	assertNoChanKeys(t, getRaw(t, h, "/api/v1/friends", aTok, 200))

	// 反查出不存在的频道/服务器：权限查询报错 → 省略不 500（故障不当有权）。
	srv.voice = stubVoice{ch: strconv.FormatInt(chID+999999, 10), sid: strconv.FormatInt(sid+999999, 10), ok: true}
	assertNoChanKeys(t, getRaw(t, h, "/api/v1/friends", aTok, 200))

	// dev 内置房间（serverID=0）：省略。
	srv.voice = stubVoice{ch: "dev-lobby", sid: "0", ok: true}
	assertNoChanKeys(t, getRaw(t, h, "/api/v1/friends", aTok, 200))

	// 恢复正常：字段回来（同一断言收口，确认前面的省略不是被永久破坏）。
	srv.voice = stubVoice{ch: strconv.FormatInt(chID, 10), sid: strconv.FormatInt(sid, 10), ok: true}
	if raw := getRaw(t, h, "/api/v1/friends", aTok, 200); !bytes.Contains(raw, []byte("channel_id")) {
		t.Fatalf("恢复正常后应带频道快照: %s", raw)
	}
}
