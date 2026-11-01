package main

// rtcscope_test.go — PR118 P2-3 的装配层验收：newRTCAuth 令牌主体缺 voice.connect
// 返回 rtcapi.ErrScopeForbidden（api 层映射 403），voice.connect 令牌与 JWT 主体
// 路径不变；走真实 /rtc/credentials handler 验证审查规格的三种状态码。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/api"
	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/rtcapi"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// TestRTCAuthRequiresVoiceConnectScope P2-3（审查规格）：messages.read 令牌请求
// /rtc/credentials 403 scope_forbidden；voice.connect 令牌 200；JWT 200。
func TestRTCAuthRequiresVoiceConnectScope(t *testing.T) {
	st := store.NewMem()
	ctx := context.Background()
	u := &store.User{ID: 1, Email: "alice@x", Nickname: "alice", Tag: "0001"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	msgTok, _ := issueAPIToken(t, st, u.ID, token.ScopeMessagesRead)
	voiceTok, _ := issueAPIToken(t, st, u.ID, token.ScopeVoiceConnect)

	jwtSigner := auth.NewJWT("test-secret-0123456789abcdef0123456789abcdef")
	apiTokenSvc := token.NewService(st, log.New(io.Discard, "", 0))
	rtcAuth := newRTCAuth(jwtSigner, apiTokenSvc, false)

	// 闭包层：缺 scope → 哨兵；有 scope / JWT → uid。
	if _, err := rtcAuth(msgTok); !errors.Is(err, rtcapi.ErrScopeForbidden) {
		t.Fatalf("messages.read 令牌 err = %v, want ErrScopeForbidden", err)
	}
	if uid, err := rtcAuth(voiceTok); err != nil || uid != "1" {
		t.Fatalf("voice.connect 令牌 = (%q, %v), want (\"1\", nil)", uid, err)
	}
	jwtTok, err := jwtSigner.SignAccess(u.ID, u.Nickname, u.Tag, "", "desktop")
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if uid, err := rtcAuth(jwtTok); err != nil || uid != "1" {
		t.Fatalf("JWT 主体 = (%q, %v), want (\"1\", nil)", uid, err)
	}

	// handler 层：三种主体各一发，状态码对齐审查规格。
	srv := api.New(api.Config{
		Store:  st,
		JWT:    jwtSigner,
		Tokens: apiTokenSvc,
		Log:    log.New(io.Discard, "", 0),
		RTC: rtcapi.Config{
			Secret:   "turn-secret",
			TurnURLs: []string{"turn:turn.example.com:3478"},
			TTL:      time.Hour,
		},
		RTCAuth: rtcAuth,
	})
	h := srv.Handler()
	post := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(msgTok); rec.Code != http.StatusForbidden {
		t.Fatalf("messages.read 令牌应 403，实际 %d body=%s", rec.Code, rec.Body.String())
	} else {
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("403 响应不是合法 JSON: %v", err)
		}
		if body["code"] != "scope_forbidden" {
			t.Fatalf("403 code = %v, want scope_forbidden", body["code"])
		}
	}
	for name, tok := range map[string]string{"voice.connect": voiceTok, "jwt": jwtTok} {
		rec := post(tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 主体应 200，实际 %d body=%s", name, rec.Code, rec.Body.String())
		}
		var resp struct {
			IceServers []map[string]any `json:"iceServers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s 响应不是合法 JSON: %v", name, err)
		}
		if len(resp.IceServers) == 0 {
			t.Fatalf("%s 主体应拿到 iceServers", name)
		}
	}
}

// TestRTCAuthDevFallbackIntact dev 兜底口径不变（P2-3 不改 dev 语义）：
// ltk_ 令牌缺 scope 仍拒绝（不落 devUserID）；非 ltk_ 的 dev- 令牌照旧映射。
func TestRTCAuthDevFallbackIntact(t *testing.T) {
	st := store.NewMem()
	ctx := context.Background()
	u := &store.User{ID: 7, Email: "bot@x", Nickname: "bot", Tag: "0007"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	msgTok, _ := issueAPIToken(t, st, u.ID, token.ScopeMessagesRead)

	jwtSigner := auth.NewJWT("test-secret-0123456789abcdef0123456789abcdef")
	rtcAuth := newRTCAuth(jwtSigner, token.NewService(st, log.New(io.Discard, "", 0)), true)

	if _, err := rtcAuth(msgTok); !errors.Is(err, rtcapi.ErrScopeForbidden) {
		t.Fatalf("dev 模式下缺 scope 的 ltk_ 令牌仍应拒绝，实际 %v", err)
	}
	uid, err := rtcAuth("dev-carol")
	if err != nil || uid != "carol" {
		t.Fatalf("dev- 令牌 = (%q, %v), want (\"carol\", nil)", uid, err)
	}
	jwtTok, err := jwtSigner.SignAccess(u.ID, u.Nickname, u.Tag, "", "desktop")
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if uid, err := rtcAuth(jwtTok); err != nil || uid != "7" {
		t.Fatalf("dev 模式 JWT = (%q, %v), want (\"7\", nil)", uid, err)
	}
}
