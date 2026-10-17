package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

func TestPasswordHash(t *testing.T) {
	hash, err := HashPassword("secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(hash, "secret-password") {
		t.Fatal("正确密码应通过")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("错误密码不应通过")
	}
}

func TestJWTSignParse(t *testing.T) {
	j := NewJWT("test-secret")
	tok, err := j.SignAccess(42, "alice", "0123", "s1", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := j.ParseAccess(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != 42 || claims.Nickname != "alice" || claims.Tag != "0123" {
		t.Fatalf("claims 不符: %+v", claims)
	}
	if claims.Sid != "s1" || claims.Dev != "desktop" {
		t.Fatalf("sid/dev 不符: %+v", claims)
	}
	// 换密钥解析应失败。
	if _, err := NewJWT("other-secret").ParseAccess(tok); err == nil {
		t.Fatal("错误密钥应拒绝")
	}
	// 旧版本令牌（无 sid/dev）兼容解析。
	oldTok, err := j.SignAccess(1, "bob", "0001", "", "")
	if err != nil {
		t.Fatal(err)
	}
	oldClaims, err := j.ParseAccess(oldTok)
	if err != nil || oldClaims.Sid != "" || oldClaims.Dev != "" {
		t.Fatalf("旧版本令牌应解析为空 sid/dev: %+v err=%v", oldClaims, err)
	}
}

// testSid / testSid2 32 hex 会话 ID（randomSid 产物形态）。
const (
	testSid  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSid2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestParseRefreshTokenFormats refresh 令牌四段格式解析与旧格式回落（R1）。
func TestParseRefreshTokenFormats(t *testing.T) {
	cases := []struct {
		name  string
		token string
		uid   int64
		dev   string
		sid   string
		ok    bool
	}{
		{"新格式四段", "7.desktop." + testSid + ".rand", 7, "desktop", testSid, true},
		{"新格式 mobile", "7.mobile." + testSid + ".rand", 7, "mobile", testSid, true},
		{"退化新格式无 sid", "7.desktop.rand", 7, "desktop", "", true},
		{"旧格式三段", "7." + testSid + ".rand", 7, "desktop", testSid, true},
		{"旧格式两段", "7.rand", 7, "desktop", "", true},
		{"无点", "deadbeef", 0, "", "", false},
		{"uid 非数字", "x.desktop." + testSid + ".rand", 0, "", "", false},
		{"uid 为零", "0.desktop.rand", 0, "", "", false},
		{"设备不在白名单", "7.tablet." + testSid + ".rand", 0, "", "", false},
		{"dev 段为空", "7.." + testSid + ".rand", 0, "", "", false},
		{"尾部多段", "7.desktop." + testSid + ".rand.extra", 0, "", "", false},
	}
	for _, c := range cases {
		uid, dev, sid, ok := ParseRefreshToken(c.token)
		if ok != c.ok || uid != c.uid || dev != c.dev || sid != c.sid {
			t.Errorf("%s: ParseRefreshToken(%q) = (%d,%q,%q,%v), want (%d,%q,%q,%v)",
				c.name, c.token, uid, dev, sid, ok, c.uid, c.dev, c.sid, c.ok)
		}
	}
}

func TestRefreshRotate(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	plain, err := rs.IssueWithUID(ctx, 7, "desktop", testSid)
	if err != nil {
		t.Fatal(err)
	}
	uid, dev, sid, ok := ParseRefreshToken(plain)
	if !ok || uid != 7 || dev != "desktop" || sid != testSid {
		t.Fatalf("解析 uid/dev/sid 失败: %d %q %q %v", uid, dev, sid, ok)
	}

	// 旋转：换发新令牌，dev/sid 保持不变。
	plain2, err := rs.Rotate(ctx, 7, plain)
	if err != nil {
		t.Fatal(err)
	}
	if plain2 == plain {
		t.Fatal("旋转后令牌应不同")
	}
	if _, dev2, sid2, ok := ParseRefreshToken(plain2); !ok || dev2 != "desktop" || sid2 != testSid {
		t.Fatalf("新令牌应保持 dev/sid: %q %q %v", dev2, sid2, ok)
	}
	// 旧令牌重放 → session_replayed（CAS 比对失败）。
	if _, err := rs.Rotate(ctx, 7, plain); !errors.Is(err, ErrSessionReplayed) {
		t.Fatalf("旧令牌旋转应 ErrSessionReplayed，实际 %v", err)
	}
	// 新令牌可继续旋转（链式刷新）。
	plain3, err := rs.Rotate(ctx, 7, plain2)
	if err != nil {
		t.Fatal(err)
	}
	// 吊销后全部失效。
	if err := rs.Revoke(ctx, 7, plain3); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Rotate(ctx, 7, plain3); err == nil {
		t.Fatal("吊销后旋转应失败")
	}
}

// TestRefreshRotateSessionReplayed 哈希不符（伪造/重放旧 rand 段）→ session_replayed。
// sid 与当前会话不符的 session_replaced 由 API 层持 SessionIndex 判定（见 api 包测试）。
func TestRefreshRotateSessionReplayed(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	if _, err := rs.IssueWithUID(ctx, 7, "desktop", testSid); err != nil {
		t.Fatal(err)
	}
	// 新格式伪造 rand 段（sid 相符但哈希不符）。
	forged := "7.desktop." + testSid + ".deadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := rs.Rotate(ctx, 7, forged); !errors.Is(err, ErrSessionReplayed) {
		t.Fatalf("session_replayed，实际 %v", err)
	}
	// 旧格式伪造同样拒绝（回落 desktop 键）。
	forgedOld := "7." + testSid + ".deadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := rs.Rotate(ctx, 7, forgedOld); !errors.Is(err, ErrSessionReplayed) {
		t.Fatalf("旧格式伪造应 session_replayed，实际 %v", err)
	}
}

// TestRefreshRotateLegacyKey 兼容旧版单槽位 rt:{uid}（v1.1.0 迁移）：
// 旧格式令牌（含/不含 sid 段）首次旋转即迁移到 rt:{uid}:{dev} 并删除旧键。
func TestRefreshRotateLegacyKey(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	// 旧版三段格式（有 sid）：直接存到 legacy 键 rt:{uid}（不带 dev 后缀）。
	legacyPlain := "9." + testSid + ".legacy-random-token"
	_ = mem.Set(ctx, rtKeyLegacy(9), HashToken("legacy-random-token"), RefreshTokenTTL)

	plain2, err := rs.Rotate(ctx, 9, legacyPlain)
	if err != nil {
		t.Fatal(err)
	}
	uid, dev, sid, ok := ParseRefreshToken(plain2)
	if !ok || uid != 9 || dev != "desktop" || sid != testSid {
		t.Fatalf("迁移后令牌应含 dev=desktop 与原 sid: %d %q %q %v", uid, dev, sid, ok)
	}
	// 旧键应已删；新键可用。
	if _, err := mem.Get(ctx, rtKeyLegacy(9)); err == nil {
		t.Fatal("旧键应已删")
	}
	if !rs.Match(ctx, 9, plain2) {
		t.Fatal("迁移后新键应匹配")
	}

	// 旧版两段格式（无 sid）：迁移后 sid 为空（退化格式 <uid>.<dev>.<rand>）。
	legacyPlain2 := "8.legacy-random-token"
	_ = mem.Set(ctx, rtKeyLegacy(8), HashToken("legacy-random-token"), RefreshTokenTTL)
	plain3, err := rs.Rotate(ctx, 8, legacyPlain2)
	if err != nil {
		t.Fatal(err)
	}
	if _, dev3, sid3, ok := ParseRefreshToken(plain3); !ok || dev3 != "desktop" || sid3 != "" {
		t.Fatalf("两段旧令牌迁移后 sid 应为空: %q %q %v", dev3, sid3, ok)
	}
}

// TestRefreshMobileLifecycle mobile 全链路（P0-2 回归）：签发→旋转→匹配→吊销，
// 且与 desktop 槽位互不干扰（修复前 refresh/logout 写死 desktop，mobile 必败）。
func TestRefreshMobileLifecycle(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	mobile, err := rs.IssueWithUID(ctx, 7, "mobile", testSid)
	if err != nil {
		t.Fatal(err)
	}
	if uid, dev, sid, ok := ParseRefreshToken(mobile); !ok || uid != 7 || dev != "mobile" || sid != testSid {
		t.Fatalf("mobile 令牌解析失败: %d %q %q %v", uid, dev, sid, ok)
	}
	// 旋转成功且仍带 mobile。
	mobile2, err := rs.Rotate(ctx, 7, mobile)
	if err != nil {
		t.Fatal(err)
	}
	if _, dev, _, _ := ParseRefreshToken(mobile2); dev != "mobile" {
		t.Fatalf("旋转后 dev 应仍为 mobile")
	}
	if !rs.Match(ctx, 7, mobile2) {
		t.Fatal("mobile 令牌应匹配")
	}

	// desktop 令牌与 mobile 槽位互不干扰。
	desktop, err := rs.IssueWithUID(ctx, 7, "desktop", testSid2)
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Match(ctx, 7, desktop) {
		t.Fatal("desktop 令牌应匹配")
	}
	// 吊销 mobile 不影响 desktop。
	if err := rs.Revoke(ctx, 7, mobile2); err != nil {
		t.Fatal(err)
	}
	if rs.Match(ctx, 7, mobile2) {
		t.Fatal("吊销后 mobile 不应匹配")
	}
	if !rs.Match(ctx, 7, desktop) {
		t.Fatal("吊销 mobile 不应影响 desktop")
	}
	// 已吊销的 mobile 令牌再旋转 → 键已没了，invalid 而非 replayed。
	if _, err := rs.Rotate(ctx, 7, mobile2); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("已吊销令牌旋转应 ErrInvalidToken，实际 %v", err)
	}
}

// TestRefreshCASConcurrent 真 CAS 回归（P2-4）：多 goroutine 同时旋转同一令牌，
// 有且只有一个成功，其余全部 ErrSessionReplayed（-race 下执行）。
func TestRefreshCASConcurrent(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	plain, err := rs.IssueWithUID(ctx, 7, "desktop", testSid)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = rs.Rotate(ctx, 7, plain)
		}(i)
	}
	wg.Wait()
	okCount := 0
	for i, err := range results {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, ErrSessionReplayed):
		default:
			t.Fatalf("goroutine %d: 期望成功或 ErrSessionReplayed，实际 %v", i, err)
		}
	}
	if okCount != 1 {
		t.Fatalf("应恰有 1 个 goroutine 旋转成功，实际 %d", okCount)
	}
}

func TestRefreshMatch(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	plain, err := rs.IssueWithUID(ctx, 7, "desktop", testSid)
	if err != nil {
		t.Fatal(err)
	}
	// 真令牌匹配。
	if !rs.Match(ctx, 7, plain) {
		t.Fatal("真令牌应匹配")
	}
	// 伪造 "<uid>.x" 不匹配。
	if rs.Match(ctx, 7, "7.x") {
		t.Fatal("伪造令牌不应匹配")
	}
	// 其他 uid 的令牌不匹配。
	if rs.Match(ctx, 8, plain) {
		t.Fatal("他人令牌不应匹配")
	}
	// 未签发过令牌的 uid 不匹配（而非报错）。
	if rs.Match(ctx, 99, "99.x") {
		t.Fatal("未签发 uid 不应匹配")
	}
	// 吊销后不再匹配。
	if err := rs.Revoke(ctx, 7, plain); err != nil {
		t.Fatal(err)
	}
	if rs.Match(ctx, 7, plain) {
		t.Fatal("吊销后不应匹配")
	}
}

// TestDeviceWhitelist 设备类型白名单（P2-2）：api.deviceType 与 auth 共用这一份。
func TestDeviceWhitelist(t *testing.T) {
	for _, d := range []string{"desktop", "mobile"} {
		if !IsKnownDevice(d) {
			t.Fatalf("%q 应在白名单", d)
		}
	}
	for _, d := range []string{"tablet", "", "Desktop", "web", "desktop ", "desktop\n"} {
		if IsKnownDevice(d) {
			t.Fatalf("%q 不应在白名单", d)
		}
	}
}

// TestRefreshRevokeAll 重置密码语义：按 DeviceTypes 白名单吊销全部设备（含 legacy 键）。
func TestRefreshRevokeAll(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	desktop, err := rs.IssueWithUID(ctx, 7, "desktop", testSid)
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := rs.IssueWithUID(ctx, 7, "mobile", testSid)
	if err != nil {
		t.Fatal(err)
	}
	_ = mem.Set(ctx, rtKeyLegacy(7), HashToken("legacy"), RefreshTokenTTL)
	if err := rs.RevokeAll(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if rs.Match(ctx, 7, desktop) || rs.Match(ctx, 7, mobile) {
		t.Fatal("RevokeAll 后全部设备应失效")
	}
	if _, err := mem.Get(ctx, rtKeyLegacy(7)); err == nil {
		t.Fatal("RevokeAll 应连 legacy 键一起删")
	}
}

func TestEmailCodeVerify(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	ec := NewEmailCodes(mem)

	code, err := ec.Send(ctx, PurposeRegister, "a@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	// 重发间隔。
	if _, err := ec.Send(ctx, PurposeRegister, "a@ex.com"); err != ErrCodeTooFrequent {
		t.Fatalf("应拒绝重发，实际 %v", err)
	}
	// 错误验证码。
	if err := ec.Verify(ctx, PurposeRegister, "a@ex.com", "000000"); err != ErrCodeInvalid {
		t.Fatalf("错误码应拒绝，实际 %v", err)
	}
	// 正确验证码通过且销毁。
	if err := ec.Verify(ctx, PurposeRegister, "a@ex.com", code); err != nil {
		t.Fatal(err)
	}
	if err := ec.Verify(ctx, PurposeRegister, "a@ex.com", code); err != ErrCodeExpired {
		t.Fatalf("验证后应销毁，实际 %v", err)
	}
}

func TestEmailCodeAttempts(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	ec := NewEmailCodes(mem)

	code, _ := ec.Send(ctx, PurposeReset, "b@ex.com")
	for i := 0; i < CodeAttempts; i++ {
		_ = ec.Verify(ctx, PurposeReset, "b@ex.com", "999999")
	}
	// 超次作废，正确码也失效。
	if err := ec.Verify(ctx, PurposeReset, "b@ex.com", code); err != ErrCodeExpired {
		t.Fatalf("超次后应作废，实际 %v", err)
	}
}

func TestEmailCodeDailyLimit(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	ec := NewEmailCodes(mem)
	for i := 0; i < CodeDailyLimit; i++ {
		// 绕过 60s 间隔：每次删掉间隔键。
		_ = mem.Del(ctx, "rl:code:interval:register:c@ex.com")
		if _, err := ec.Send(ctx, PurposeRegister, "c@ex.com"); err != nil {
			t.Fatalf("第 %d 次发送失败: %v", i+1, err)
		}
	}
	_ = mem.Del(ctx, "rl:code:interval:register:c@ex.com")
	if _, err := ec.Send(ctx, PurposeRegister, "c@ex.com"); err != ErrCodeDailyLimit {
		t.Fatalf("超每日限次应拒绝，实际 %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rl := NewRateLimit(mem)
	for i := 0; i < 5; i++ {
		ok, err := rl.Allow(ctx, "login:1.2.3.4", 5, time.Minute)
		if err != nil || !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	ok, _ := rl.Allow(ctx, "login:1.2.3.4", 5, time.Minute)
	if ok {
		t.Fatal("第 6 次应拒绝")
	}
}
