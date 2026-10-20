package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
)

// #36 服务端强制闭麦 REST：权限门、频道形态、自我操作、目标不在房与审计。

// fakeHub 记录 SetServerMuted 调用；按 "channelID|userID" 模拟目标不在房。
type fakeHub struct {
	mu    sync.Mutex
	calls []string
	notIn map[string]bool
}

func (f *fakeHub) Kick(uid, dev, reason string)         {}
func (f *fakeHub) KickChannel(channelID, reason string) {}

func (f *fakeHub) SetServerMuted(channelID, userID string, muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%v", channelID, userID, muted))
	if f.notIn[channelID+"|"+userID] {
		return signal.ErrNotInChannel
	}
	return nil
}

func (f *fakeHub) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

// setupServerMute 一套闭麦测试夹具：owner + member + 语音频道 + fake hub。
func setupServerMute(t *testing.T) (w *permWorld, h *fakeHub, voiceID, memberID int64) {
	t.Helper()
	w = newPermWorld(t)
	h = &fakeHub{notIn: map[string]bool{}}
	w.srv.hub = h
	ch := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "语音房", "type": "voice"}, http.StatusCreated)
	return w, h, asID(ch["id"]), meID(t, w.h, w.member)
}

// TestServerMuteAPI 权限与错误映射 + 成功路径审计。
func TestServerMuteAPI(t *testing.T) {
	w, h, voiceID, memberID := setupServerMute(t)
	rt := w.h
	body := map[string]bool{"muted": true}
	path := fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", voiceID, memberID)

	// 成员无 MuteMembers → 403；非成员 → 403。
	doJSON(t, rt, http.MethodPut, path, w.member, body, http.StatusForbidden)
	doJSON(t, rt, http.MethodPut, path, w.stranger, body, http.StatusForbidden)

	// 频道不存在 404。
	doJSON(t, rt, http.MethodPut, fmt.Sprintf("/api/v1/channels/4242424242/members/%d/server-mute", memberID),
		w.owner, body, http.StatusNotFound)

	// 非语音频道 400（先有权限，owner 过权限门）。
	text := postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "文字", "type": "text"}, http.StatusCreated)
	doJSON(t, rt, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", asID(text["id"]), memberID),
		w.owner, body, http.StatusBadRequest)

	// 自我操作 400。
	ownerID := meID(t, rt, w.owner)
	doJSON(t, rt, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", voiceID, ownerID),
		w.owner, body, http.StatusBadRequest)

	// 目标不在语音房 404。
	h.notIn[strconv.FormatInt(voiceID, 10)+"|"+strconv.FormatInt(memberID, 10)] = true
	doJSON(t, rt, http.MethodPut, path, w.owner, body, http.StatusNotFound)

	// 成功：hub 收到 muted=true，审计 voice.server_mute。
	delete(h.notIn, strconv.FormatInt(voiceID, 10)+"|"+strconv.FormatInt(memberID, 10))
	doJSON(t, rt, http.MethodPut, path, w.owner, body, http.StatusOK)
	if got := h.last(); got != fmt.Sprintf("%d|%d|true", voiceID, memberID) {
		t.Fatalf("hub 调用 = %q", got)
	}
	// 解禁。
	doJSON(t, rt, http.MethodPut, path, w.owner, map[string]bool{"muted": false}, http.StatusOK)
	if got := h.last(); !strings.HasSuffix(got, "|false") {
		t.Fatalf("解禁应 muted=false: %q", got)
	}

	// 审计：voice.server_mute / voice.server_unmute 都有，detail 无隐私字段。
	logs := getJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.owner, http.StatusOK).([]any)
	seen := map[string]bool{}
	for _, raw := range logs {
		e := raw.(map[string]any)
		action, _ := e["action"].(string)
		if action == "voice.server_mute" || action == "voice.server_unmute" {
			seen[action] = true
			detail, _ := e["detail"].(string)
			if strings.Contains(detail, "email") || strings.Contains(detail, "token") {
				t.Fatalf("审计 detail 不得含隐私字段: %s", detail)
			}
		}
	}
	if !seen["voice.server_mute"] || !seen["voice.server_unmute"] {
		t.Fatalf("审计应含 mute/unmute 两类，实际 %v", seen)
	}

	// 非法 body 400（muted 类型错配，解码失败）。
	doJSON(t, rt, http.MethodPut, path, w.owner, map[string]any{"muted": "yes"}, http.StatusBadRequest)
}

// TestServerMuteHubNil 无信令面时按「目标不在房」404（fail-closed，不 500）。
func TestServerMuteHubNil(t *testing.T) {
	w := newPermWorld(t)
	ch := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "语音", "type": "voice"}, http.StatusCreated)
	memberID := meID(t, w.h, w.member)
	doJSON(t, w.h, http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%d/members/%d/server-mute", asID(ch["id"]), memberID),
		w.owner, map[string]bool{"muted": true}, http.StatusNotFound)
}
