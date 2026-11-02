package main

// token_session_test.go — PR118 P2-2 的装配层验收：apitokenSessionChecker 把
// store 的令牌行只读查询适配成 hub 的 signal.TokenSessionChecker，供 WS 心跳
// 按 tokenID 复查吊销/过期。

import (
	"context"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// issueAPIToken 建行并返回明文（测试直写 store，绕过 REST）。
func issueAPIToken(t *testing.T, st *store.Mem, userID int64, scopes string) (plain string, id int64) {
	t.Helper()
	p, err := token.NewPlain(token.KindPAT)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	row := &store.APIToken{
		UserID:     userID,
		Name:       "test",
		Prefix:     p.Prefix,
		SecretHash: p.Hash,
		Kind:       token.KindPAT,
		Scopes:     scopes,
	}
	if err := st.CreateAPIToken(context.Background(), row); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return p.Token, row.ID
}

// TestAPITokenSessionChecker P2-2 适配器：有效→true；吊销/过期/行不存在→false；
// 其余查询故障原样上抛（hub 侧 fail-open 的判据）。
func TestAPITokenSessionChecker(t *testing.T) {
	st := store.NewMem()
	ctx := context.Background()
	u := &store.User{Email: "chk@x", Nickname: "chk", Tag: "0001"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, id := issueAPIToken(t, st, u.ID, token.ScopeMessagesRead)
	chk := apitokenSessionChecker{st: st}

	if ok, err := chk.SessionValid(ctx, id); err != nil || !ok {
		t.Fatalf("有效令牌应 (true, nil)，实际 (%v, %v)", ok, err)
	}
	if err := st.RevokeAPIToken(ctx, id, time.Now()); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if ok, err := chk.SessionValid(ctx, id); err != nil || ok {
		t.Fatalf("吊销后应 (false, nil)，实际 (%v, %v)", ok, err)
	}
	if ok, err := chk.SessionValid(ctx, 999999); err != nil || ok {
		t.Fatalf("行不存在应 (false, nil)，实际 (%v, %v)", ok, err)
	}

	// 过期令牌：ExpiresAt 已过（握手会拒，但复查口径也要独立成立）。
	p, err := token.NewPlain(token.KindPAT)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	expired := &store.APIToken{
		UserID: u.ID, Name: "expired", Prefix: p.Prefix, SecretHash: p.Hash,
		Kind: token.KindPAT, Scopes: token.ScopeMessagesRead, ExpiresAt: &past,
	}
	if err := st.CreateAPIToken(ctx, expired); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if ok, err := chk.SessionValid(ctx, expired.ID); err != nil || ok {
		t.Fatalf("过期令牌应 (false, nil)，实际 (%v, %v)", ok, err)
	}
}
