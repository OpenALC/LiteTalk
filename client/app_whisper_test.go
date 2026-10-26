package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/hotkey"
)

// #45：悄悄话旁路门与优先发言授予的客户端逻辑。

// TestWhisperGateFlow 按住悄悄话键在无目标时不开麦、有目标时旁路放行；
// 释放恢复 PTT 语义。发送到会话的部分在无会话时为空操作，不 panic。
func TestWhisperGateFlow(t *testing.T) {
	app := &App{hkGate: &hotkey.Gate{}}
	app.hkGate.SetMode(hotkey.ModePTT)

	// 无目标：按键不生效。
	_ = app.SetWhisperTargets(nil)
	app.whisperPress()
	if app.hkGate.Allow(false) {
		t.Fatal("无目标时悄悄话不应开麦")
	}
	app.whisperRelease()

	// 有目标：按住放行，松开恢复 PTT 收门。
	_ = app.SetWhisperTargets([]string{"u1", "u1", ""})
	app.whisperPress()
	if !app.hkGate.Allow(false) {
		t.Fatal("有目标时按住悄悄话应放行上行")
	}
	app.whisperRelease()
	if app.hkGate.Allow(false) {
		t.Fatal("释放悄悄话后应恢复 PTT 收门")
	}

	// 按住期间清空目标：立即关旁路门（回到 PTT 收门）。
	app.whisperPress()
	_ = app.SetWhisperTargets([]string{})
	if app.hkGate.Allow(false) {
		t.Fatal("按住期间清空目标应立即关麦")
	}
	app.whisperRelease()
}

// TestWhisperGateCoexistsWithPTT 先松 PTT 再松悄悄话（或相反）都不能误关麦。
func TestWhisperGateCoexistsWithPTT(t *testing.T) {
	app := &App{hkGate: &hotkey.Gate{}}
	app.hkGate.SetMode(hotkey.ModePTT)
	_ = app.SetWhisperTargets([]string{"u1"})

	app.hkGate.Press()
	app.whisperPress()
	app.hkGate.Release(0) // 松开 PTT：悄悄话仍按住，必须继续放行
	if !app.hkGate.Allow(false) {
		t.Fatal("悄悄话按住时松开 PTT 不得关麦")
	}
	app.whisperRelease()
	if app.hkGate.Allow(false) {
		t.Fatal("两者都释放后应收门")
	}
}

// priorityServer 覆盖接口桩：GET 返回 current；PUT/DELETE 记录请求并更新 current。
type priorityServer struct {
	mu      sync.Mutex
	current string
	puts    []string // "allow|deny"
	deletes int
	fail    int
}

func (p *priorityServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, p.current)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Allow string `json:"allow"`
				Deny  string `json:"deny"`
			}
			_ = json.Unmarshal(body, &req)
			p.puts = append(p.puts, req.Allow+"|"+req.Deny)
			if p.fail != 0 {
				w.WriteHeader(p.fail)
				_, _ = io.WriteString(w, `{"code":"forbidden","detail":"forbidden"}`)
				return
			}
			p.current = fmt.Sprintf(`[{"channel_id":"7","target_type":"member","target_id":"3","allow":%q,"deny":%q,"updated_at":"2026-01-01T00:00:00Z"}]`, req.Allow, req.Deny)
			_, _ = io.WriteString(w, strings.TrimSuffix(strings.TrimPrefix(p.current, "["), "]"))
		case http.MethodDelete:
			p.deletes++
			p.current = "[]"
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// TestSetPrioritySpeaker 保留其他权限位、撤销不写 deny、空覆盖删除、无覆盖撤销为空操作。
func TestSetPrioritySpeaker(t *testing.T) {
	cases := []struct {
		name       string
		current    string
		enabled    bool
		wantPut    string // "" = 不应 PUT
		wantDelete int
	}{
		{
			name:    "授予保留其他位",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"8","deny":"2","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: true, wantPut: "24|2",
		},
		{
			name:    "撤销只清 allow 位",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"16","deny":"2","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false, wantPut: "0|2",
		},
		{
			name:    "撤销后覆盖为空则删除",
			current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false, wantDelete: 1,
		},
		{
			name:    "无个人覆盖授予",
			current: `[{"channel_id":"7","target_type":"role","target_id":"9","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: true, wantPut: "16|0",
		},
		{
			name:    "无个人覆盖撤销是空操作",
			current: `[{"channel_id":"7","target_type":"role","target_id":"9","allow":"16","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
			enabled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &priorityServer{current: tc.current}
			srv := httptest.NewServer(stub.handler())
			defer srv.Close()
			app := &App{api: newAppAPI(srv.URL)}

			if err := app.SetPrioritySpeaker("7", "3", tc.enabled); err != nil {
				t.Fatalf("SetPrioritySpeaker: %v", err)
			}
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if tc.wantPut == "" {
				if len(stub.puts) != 0 {
					t.Fatalf("不应 PUT，实际 %v", stub.puts)
				}
			} else if len(stub.puts) != 1 || stub.puts[0] != tc.wantPut {
				t.Fatalf("PUT = %v，期望 %q", stub.puts, tc.wantPut)
			}
			if stub.deletes != tc.wantDelete {
				t.Fatalf("DELETE 次数 = %d，期望 %d", stub.deletes, tc.wantDelete)
			}
		})
	}
}

// TestSetPrioritySpeakerForbidden 403 原样返回，前端提示权限不足。
func TestSetPrioritySpeakerForbidden(t *testing.T) {
	stub := &priorityServer{
		current: `[{"channel_id":"7","target_type":"member","target_id":"3","allow":"0","deny":"0","updated_at":"2026-01-01T00:00:00Z"}]`,
		fail:    http.StatusForbidden,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	app := &App{api: newAppAPI(srv.URL)}
	if err := app.SetPrioritySpeaker("7", "3", true); err == nil {
		t.Fatal("403 应返回错误")
	}
}
