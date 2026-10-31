package token

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Mem, *time.Time) {
	t.Helper()
	mem := store.NewMem()
	svc := NewService(mem, nil)
	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	return svc, mem, &now
}

func mustUser(t *testing.T, mem *store.Mem, email, nickname string, status int8) *store.User {
	t.Helper()
	u := &store.User{Email: email, Nickname: nickname, Tag: "0001", Status: status}
	if err := mem.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func mustToken(t *testing.T, mem *store.Mem, userID int64, scopes string) (*store.APIToken, *Plain) {
	t.Helper()
	p, err := NewPlain(KindPAT)
	if err != nil {
		t.Fatalf("NewPlain: %v", err)
	}
	row := &store.APIToken{
		UserID:     userID,
		Name:       "测试",
		Prefix:     p.Prefix,
		SecretHash: p.Hash,
		Kind:       KindPAT,
		Scopes:     scopes,
	}
	if err := mem.CreateAPIToken(context.Background(), row); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return row, p
}

// TestNewPlainFormat 明文形态、前缀字符集与哈希一致性；多次生成不撞车。
func TestNewPlainFormat(t *testing.T) {
	shape := regexp.MustCompile(`^ltk_pat_([0-9a-f]{8})_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		p, err := NewPlain(KindPAT)
		if err != nil {
			t.Fatalf("NewPlain: %v", err)
		}
		m := shape.FindStringSubmatch(p.Token)
		if m == nil {
			t.Fatalf("明文形态不符: %q", p.Token)
		}
		if m[1] != p.Prefix {
			t.Fatalf("明文里的前缀 %s 与 Prefix %s 不一致", m[1], p.Prefix)
		}
		// secret 段：前缀 "ltk_pat_" (8) + prefix (8) + 分隔 '_' 之后；base64url
		// 字母表含 '_'，不能按最后一个下划线切。
		secret := p.Token[len("ltk_pat_")+8+1:]
		if p.Hash != HashSecret(secret) {
			t.Fatal("Hash 与明文里的 secret 段不一致")
		}
		if seen[p.Prefix] {
			t.Fatalf("prefix 重复: %s", p.Prefix)
		}
		seen[p.Prefix] = true
	}
	if _, err := NewPlain("jwt"); err == nil {
		t.Fatal("jwt 不是可签发种类")
	}
}

// TestVerifyRoundTrip 正常令牌 → 与 JWT 同构的 Principal，属主身份正确，
// last_used_at 异步落库。
func TestVerifyRoundTrip(t *testing.T) {
	svc, mem, now := newTestService(t)
	u := mustUser(t, mem, "v@ex.com", "verifier", 0)
	row, plain := mustToken(t, mem, u.ID, "messages.read messages.write")

	ctx := context.Background()
	p, err := svc.Verify(ctx, plain.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Claims == nil || p.UserID != u.ID || p.Nickname != u.Nickname || p.Tag != u.Tag {
		t.Fatalf("Principal 身份不符: %+v", p.Claims)
	}
	if p.Kind != KindPAT || p.TokenID != row.ID {
		t.Fatalf("Kind/TokenID 不符: kind=%s id=%d", p.Kind, p.TokenID)
	}
	if !p.HasScope("messages.read") || !p.HasScope("messages.write") || p.HasScope("admin.read") {
		t.Fatalf("Scopes 不符: %v", p.Scopes)
	}
	if p.Sid != "" || p.Dev != "" {
		t.Fatalf("令牌 Principal 无会话概念：sid=%q dev=%q", p.Sid, p.Dev)
	}
	svc.WaitIdle()
	got, err := mem.APITokenByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(*now) {
		t.Fatalf("last_used_at 应已异步落库：got=%v want=%v", got.LastUsedAt, *now)
	}
}

// TestVerifyFailures 错误 secret / 未知前缀 / 畸形明文 全部拒绝且不区分原因。
func TestVerifyFailures(t *testing.T) {
	svc, mem, _ := newTestService(t)
	ctx := context.Background()
	u := mustUser(t, mem, "f@ex.com", "failer", 0)
	_, plain := mustToken(t, mem, u.ID, "messages.read")
	good := plain.Token

	// 篡改 secret 段（等长）。
	bad := []byte(good)
	bad[len(bad)-1] ^= 1
	if _, err := svc.Verify(ctx, string(bad)); err != ErrInvalidToken {
		t.Fatalf("错 secret 应 ErrInvalidToken，实际 %v", err)
	}
	// 未知前缀。
	if _, err := svc.Verify(ctx, "ltk_pat_00000000_"+good[len(good)-43:]); err != ErrInvalidToken {
		t.Fatalf("未知前缀应 ErrInvalidToken，实际 %v", err)
	}
	// 畸形明文（空串、缺段、大写前缀、错 kind）。
	for _, s := range []string{"", "ltk_pat_", "ltk_pat_XXXXXXXX_" + good[len(good)-43:],
		"LTK_PAT_0123abcd_" + good[len(good)-43:], "ltk_jwt_0123abcd_" + good[len(good)-43:]} {
		if _, err := svc.Verify(ctx, s); err != ErrInvalidToken {
			t.Fatalf("畸形 %q 应 ErrInvalidToken，实际 %v", s, err)
		}
	}
}

// TestVerifyRevokedImmediate 吊销立即生效：无缓存，下一次校验即拒绝。
func TestVerifyRevokedImmediate(t *testing.T) {
	svc, mem, now := newTestService(t)
	ctx := context.Background()
	u := mustUser(t, mem, "r@ex.com", "revoker", 0)
	row, plain := mustToken(t, mem, u.ID, "messages.read")

	if _, err := svc.Verify(ctx, plain.Token); err != nil {
		t.Fatalf("吊销前应有效: %v", err)
	}
	if err := mem.RevokeAPIToken(ctx, row.ID, *now); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, err := svc.Verify(ctx, plain.Token); err != ErrInvalidToken {
		t.Fatalf("吊销后应 ErrInvalidToken，实际 %v", err)
	}
}

// TestVerifyExpired 过期令牌拒绝。
func TestVerifyExpired(t *testing.T) {
	svc, mem, now := newTestService(t)
	ctx := context.Background()
	u := mustUser(t, mem, "e@ex.com", "expirer", 0)
	p, err := NewPlain(KindPAT)
	if err != nil {
		t.Fatal(err)
	}
	exp := now.Add(time.Hour)
	if err := mem.CreateAPIToken(ctx, &store.APIToken{UserID: u.ID, Prefix: p.Prefix,
		SecretHash: p.Hash, Kind: KindPAT, Scopes: "messages.read", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, p.Token); err != nil {
		t.Fatalf("未过期应有效: %v", err)
	}
	*now = now.Add(time.Hour + time.Second)
	if _, err := svc.Verify(ctx, p.Token); err != ErrInvalidToken {
		t.Fatalf("过期后应 ErrInvalidToken，实际 %v", err)
	}
}

// TestVerifyOwnerStatus 属主被停用/不存在后令牌失效（与 JWT 路径同一口径）。
func TestVerifyOwnerStatus(t *testing.T) {
	svc, mem, _ := newTestService(t)
	ctx := context.Background()
	u := mustUser(t, mem, "s@ex.com", "stated", 0)
	_, plain := mustToken(t, mem, u.ID, "messages.read")

	u.Status = 1
	if err := mem.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, plain.Token); err != ErrInvalidToken {
		t.Fatalf("属主停用后应 ErrInvalidToken，实际 %v", err)
	}

	// 属主行不存在的悬空令牌。
	p, err := NewPlain(KindPAT)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateAPIToken(ctx, &store.APIToken{UserID: 999999,
		Prefix: p.Prefix, SecretHash: p.Hash, Kind: KindPAT, Scopes: "messages.read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, p.Token); err != ErrInvalidToken {
		t.Fatalf("属主不存在应 ErrInvalidToken，实际 %v", err)
	}
}

// TestLastUsedThrottle 节流：60s 窗口内重复使用只落库一次，过窗后再写。
func TestLastUsedThrottle(t *testing.T) {
	svc, mem, now := newTestService(t)
	ctx := context.Background()
	u := mustUser(t, mem, "t@ex.com", "throttled", 0)
	p, err := NewPlain(KindPAT)
	if err != nil {
		t.Fatal(err)
	}
	row := &store.APIToken{UserID: u.ID, Prefix: p.Prefix, SecretHash: p.Hash,
		Kind: KindPAT, Scopes: "messages.read"}
	if err := mem.CreateAPIToken(ctx, row); err != nil {
		t.Fatal(err)
	}

	svc.scheduleTouch(row.ID, *now)
	svc.WaitIdle()
	svc.scheduleTouch(row.ID, now.Add(30*time.Second)) // 窗口内：跳过
	svc.WaitIdle()

	got, err := mem.APITokenByID(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(*now) {
		t.Fatalf("应只记第一次的 now：got=%v want=%v", got.LastUsedAt, *now)
	}

	svc.scheduleTouch(row.ID, now.Add(61*time.Second)) // 过窗：再写
	svc.WaitIdle()
	got, _ = mem.APITokenByID(ctx, row.ID)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(now.Add(61*time.Second)) {
		t.Fatalf("过窗后应更新：got=%v", got.LastUsedAt)
	}
}

// TestScopes 白名单映射、创建校验与 scope 上限判定。
func TestScopes(t *testing.T) {
	if !ValidScopes([]string{ScopeMessagesRead, ScopeMessagesWrite}) {
		t.Fatal("合法 scopes 应通过")
	}
	for _, bad := range [][]string{
		nil, {},
		{""},                               // 空 scope = 无权限，拒绝
		{"messages.read", "nope"},          // 未知
		{"messages.read", "messages.read"}, // 重复
	} {
		if ValidScopes(bad) {
			t.Fatalf("%v 应拒绝", bad)
		}
	}
	if ScopeBits([]string{ScopeMessagesRead}) != store.ViewChannel {
		t.Fatal("messages.read → ViewChannel")
	}
	if ScopeBits([]string{ScopeMessagesWrite}) != store.ViewChannel|store.SendMessages {
		t.Fatal("messages.write → ViewChannel|SendMessages")
	}
	if ScopeBits([]string{ScopeVoiceConnect}) != store.ViewChannel|store.ConnectVoice|store.Speak {
		t.Fatal("voice.connect → ViewChannel|ConnectVoice|Speak")
	}
	if ScopeBits([]string{ScopeGuildManage}) != store.ManageChannels|store.ManageRoles|store.ManageServer {
		t.Fatal("guild.manage → ManageChannels|ManageRoles|ManageServer")
	}
	if ScopeBits([]string{ScopeAdminRead}) != store.ManageServer {
		t.Fatal("admin.read → ManageServer")
	}
	if ScopeBits([]string{ScopeAttachmentsWrite}) != store.AttachFiles {
		t.Fatal("attachments.write → AttachFiles")
	}
	if ScopeBits([]string{"unknown"}) != 0 {
		t.Fatal("未知 scope 贡献 0 位")
	}
}

// TestScopeLimitFromContext authz 集中判定点取上限的口径：
// JWT 主体与无 Principal 不受限；令牌主体恒受限（空 scope = 无任何权限）。
func TestScopeLimitFromContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := ScopeLimitFromContext(ctx); ok {
		t.Fatal("无 Principal 不受限")
	}
	jwt := &Principal{Claims: nil, Kind: KindJWT}
	if _, ok := ScopeLimitFromContext(WithPrincipal(ctx, jwt)); ok {
		t.Fatal("JWT 主体不受限")
	}
	pat := &Principal{Kind: KindPAT, Scopes: []string{ScopeMessagesRead}}
	limit, ok := ScopeLimitFromContext(WithPrincipal(ctx, pat))
	if !ok || limit != store.ViewChannel {
		t.Fatalf("令牌主体应受限：limit=%v ok=%v", limit, ok)
	}
	empty := &Principal{Kind: KindPAT, Scopes: nil}
	limit, ok = ScopeLimitFromContext(WithPrincipal(ctx, empty))
	if !ok || limit != 0 {
		t.Fatalf("空 scope 令牌 = 无任何权限：limit=%v ok=%v", limit, ok)
	}
}
