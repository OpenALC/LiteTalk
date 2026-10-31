package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// #102 voice → text 切换的选项清理：密码与人数上限随类型一起清掉，
// 否则切回 voice 会复活早已不该生效的旧密码。

// TestChannelTypeSwitchClearsOptions 切成 text 时清空 password/user_limit，
// 切回 voice 后旧密码不得复活。
func TestChannelTypeSwitchClearsOptions(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h

	const secret = "pw-voice-only"
	ch := w.createCh(t, map[string]any{
		"name": "会切类型", "type": "voice", "password": secret, "user_limit": 5,
	})
	chID := asID(ch["id"])
	if ch["has_password"] != true || ch["user_limit"] != float64(5) {
		t.Fatalf("初始选项不符: %v", ch)
	}

	// 切成文字频道：密码与人数上限随类型一起清掉。
	toText := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"type": "text"}, http.StatusOK)
	if toText["has_password"] != false {
		t.Fatalf("切 text 后 has_password 应 false: %v", toText)
	}
	if toText["user_limit"] != float64(0) {
		t.Fatalf("切 text 后 user_limit 应 0: %v", toText)
	}

	// 落库值同样清空（不能只改响应）。
	row, err := w.mem.ChannelByID(context.Background(), chID)
	if err != nil {
		t.Fatalf("ChannelByID: %v", err)
	}
	if row.PasswordHash != "" || row.UserLimit != 0 {
		t.Fatalf("库里仍残留旧选项：hash=%q limit=%d", row.PasswordHash, row.UserLimit)
	}

	// 切回 voice：旧密码不得复活。
	back := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"type": "voice"}, http.StatusOK)
	if back["has_password"] != false {
		t.Fatalf("切回 voice 后旧密码不得复活: %v", back)
	}
	if back["user_limit"] != float64(0) {
		t.Fatalf("切回 voice 后人数上限应为 0: %v", back)
	}

	// 审计 detail 不得含明文或哈希。
	logs := getJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.owner, http.StatusOK).([]any)
	for _, raw := range logs {
		detail, _ := raw.(map[string]any)["detail"].(string)
		if strings.Contains(detail, secret) || strings.Contains(detail, "password_hash") {
			t.Fatalf("审计 detail 泄密: %s", detail)
		}
	}
}

// TestChannelTypeSwitchRejectsPassword 同一次 PATCH 里「改成 text + 设密码」仍按
// 最终类型拒绝（清理只作用于已经存在的旧值，不能成为绕过校验的口子）。
func TestChannelTypeSwitchRejectsPassword(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h
	ch := w.createCh(t, map[string]any{"name": "语音", "type": "voice"})
	chID := asID(ch["id"])

	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"type": "text", "password": "x"}, http.StatusBadRequest)
	// 请求被拒后库里不应有半写：类型仍是 voice。
	row, err := w.mem.ChannelByID(context.Background(), chID)
	if err != nil {
		t.Fatalf("ChannelByID: %v", err)
	}
	if row.Type != "voice" {
		t.Fatalf("被拒的 PATCH 不得改动频道类型：%v", row.Type)
	}
}
