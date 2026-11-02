package main

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestRevokeBotRevokesToken `bot revoke --prefix` 的验收（#119）：吊销落库后
// token.Service.Verify 立即拒绝该明文（读路径无缓存），重复吊销幂等且不改
// 首次吊销时间。
func TestRevokeBotRevokesToken(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	user, plain, row, err := createBot(ctx, st, "看板娘", []string{token.ScopeMessagesRead})
	if err != nil {
		t.Fatalf("createBot: %v", err)
	}
	_ = user
	at := time.Unix(1_700_000_000, 0)
	got, already, err := revokeBot(ctx, st, plain.Prefix, at)
	if err != nil {
		t.Fatalf("revokeBot: %v", err)
	}
	if already || got.RevokedAt == nil || !got.RevokedAt.Equal(at) {
		t.Fatalf("首次吊销应写入 revoked_at: already=%v row=%+v", already, got)
	}
	back, err := st.APITokenByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("回读令牌行: %v", err)
	}
	if back.RevokedAt == nil || !back.RevokedAt.Equal(at) {
		t.Fatalf("吊销必须落库: %+v", back)
	}
	// 与 REST/WS 共用的同一条校验实现必须立即拒绝。
	svc := token.NewService(st, nil)
	if _, err := svc.Verify(ctx, plain.Token); err != token.ErrInvalidToken {
		t.Fatalf("吊销后 Verify 应 ErrInvalidToken，实际 %v", err)
	}

	// 幂等：第二次不改已落库的吊销时间，返回 already=true。
	got2, already2, err := revokeBot(ctx, st, plain.Prefix, at.Add(time.Hour))
	if err != nil || !already2 {
		t.Fatalf("重复吊销应幂等成功: already=%v err=%v", already2, err)
	}
	if got2.RevokedAt == nil || !got2.RevokedAt.Equal(at) {
		t.Fatalf("重复吊销不得改写首次吊销时间: %v", got2.RevokedAt)
	}
}

// TestRevokeBotRejectsNonBot prefix 形态、未知 prefix、非 bot 令牌三类拒绝：
// 命令只管 bot，pat 由持有者走 REST 自毁；拒绝路径不得改动任何行。
func TestRevokeBotRejectsNonBot(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	u := &store.User{Email: "pat@x", Nickname: "pat", Tag: "0001"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	p, err := token.NewPlain(token.KindPAT)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	row := &store.APIToken{
		UserID: u.ID, Name: "pat", Prefix: p.Prefix, SecretHash: p.Hash,
		Kind: token.KindPAT, Scopes: token.ScopeMessagesRead,
	}
	if err := st.CreateAPIToken(ctx, row); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if _, _, err := revokeBot(ctx, st, p.Prefix, time.Now()); err == nil {
		t.Fatal("bot revoke 不应吊销 pat 令牌")
	}
	back, err := st.APITokenByID(ctx, row.ID)
	if err != nil || back.RevokedAt != nil {
		t.Fatalf("拒绝路径不得改动令牌行: %+v err=%v", back, err)
	}

	// 未落库的 prefix（形态合法）→ 不存在。
	unknown, err := token.NewPlain(token.KindBot)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	if _, _, err := revokeBot(ctx, st, unknown.Prefix, time.Now()); err == nil {
		t.Fatal("未知 prefix 应报错")
	}
	// 形态不合法的 prefix 在查库前拒绝。
	for _, bad := range []string{"", "ABCDEF12", "1234abc", "123456789"} {
		if _, _, err := revokeBot(ctx, st, bad, time.Now()); err == nil {
			t.Fatalf("非法 prefix %q 应拒绝", bad)
		}
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
