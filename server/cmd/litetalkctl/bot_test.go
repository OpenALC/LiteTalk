package main

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// TestCreateBotPlainVerifiable litetalkctl bot create 的核心验收（#51）：
// 产出的明文必须是 ltk_bot_ 前缀、且能被 T2 的同一条 token.Service.Verify 校验通过。
func TestCreateBotPlainVerifiable(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	user, plain, row, err := createBot(ctx, st, "看板娘", []string{token.ScopeMessagesRead, token.ScopeMessagesWrite})
	if err != nil {
		t.Fatalf("createBot: %v", err)
	}

	// 明文形态：ltk_bot_ 前缀 + 8 位 hex prefix（与库里的行一致）。
	if !strings.HasPrefix(plain.Token, "ltk_bot_") {
		t.Fatalf("明文应为 ltk_bot_ 前缀: %s", plain.Token)
	}
	if plain.Prefix != row.Prefix || len(plain.Prefix) != 8 {
		t.Fatalf("prefix 8 位 hex 且与行一致: %q vs %q", plain.Prefix, row.Prefix)
	}

	// 可校验：与 REST/WS 共用的同一条 Verify 必须认这个明文（kind/user/scope 全对上）。
	svc := token.NewService(st, nil)
	p, err := svc.Verify(ctx, plain.Token)
	if err != nil {
		t.Fatalf("token.Service.Verify 应通过: %v", err)
	}
	if p.Kind != token.KindBot || p.UserID != user.ID {
		t.Fatalf("Principal 应为 bot 且属主正确: kind=%s uid=%d", p.Kind, p.UserID)
	}
	if len(p.Scopes) != 2 {
		t.Fatalf("scope 应为传入的两个: %v", p.Scopes)
	}

	// bot 账号形状：is_bot=1、空哈希（登录天然拒绝）、bots.invalid 保留域邮箱。
	u, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !u.IsBot {
		t.Fatal("bot 账号必须 is_bot=1")
	}
	if u.PasswordHash != "" {
		t.Fatalf("bot 的 password_hash 必须为空串: %q", u.PasswordHash)
	}
	if auth.CheckPassword(u.PasswordHash, "whatever-password") {
		t.Fatal("空哈希必须登录不了（CheckPassword 恒 false）")
	}
	if !strings.HasSuffix(u.Email, "@bots.invalid") {
		t.Fatalf("bot 邮箱必须落在保留域: %s", u.Email)
	}
	if u.Email == "@bots.invalid" || !strings.HasSuffix(u.Email, plain.Prefix+"@bots.invalid") {
		t.Fatalf("bot 邮箱前缀应取令牌 prefix 保证唯一: %s", u.Email)
	}

	// 库里落的是 kind=bot 的令牌行，scope 空格拼接原样。
	if row.Kind != token.KindBot || row.Scopes != "messages.read messages.write" {
		t.Fatalf("令牌行 kind/scopes: %s %q", row.Kind, row.Scopes)
	}
}

// TestCreateBotRejectsBadInput 名字与 scope 在创建端收口：非法值不落任何行。
func TestCreateBotRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()

	cases := []struct {
		name   string
		scopes []string
	}{
		{"a", []string{token.ScopeMessagesRead}},                     // 名字过短
		{strings.Repeat("长", 17), []string{token.ScopeMessagesRead}}, // 名字过长
		{"机器人", nil}, // 空 scope = 无权限，fail-closed
		{"机器人", []string{"messages.read", "messages.read"}}, // 重复
		{"机器人", []string{"god.mode"}},                       // 白名单外
	}
	for i, c := range cases {
		if _, _, _, err := createBot(ctx, st, c.name, c.scopes); err == nil {
			t.Fatalf("case %d 应拒绝: name=%q scopes=%v", i, c.name, c.scopes)
		}
	}
	// 拒绝路径不落任何用户行。
	if has, err := st.HasUsers(ctx); err != nil || has {
		t.Fatalf("非法入参不应落任何用户行: has=%v err=%v", has, err)
	}
}

// TestParseScopes --scopes 拆分：逗号与空格混用、全空串拒绝。
func TestParseScopes(t *testing.T) {
	got, err := parseScopes("messages.read, messages.write")
	if err != nil || len(got) != 2 {
		t.Fatalf("逗号分隔应可解析: %v %v", got, err)
	}
	if _, err := parseScopes("  "); err == nil {
		t.Fatal("空 scopes 必须拒绝（空 = 无权限，不是全权限）")
	}
}
