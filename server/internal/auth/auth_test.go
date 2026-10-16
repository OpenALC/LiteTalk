package auth

import (
	"context"
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
	tok, err := j.SignAccess(42, "alice", "0123")
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
	// 换密钥解析应失败。
	if _, err := NewJWT("other-secret").ParseAccess(tok); err == nil {
		t.Fatal("错误密钥应拒绝")
	}
}

func TestRefreshRotate(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	plain, err := rs.IssueWithUID(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := ParseRefreshUID(plain)
	if !ok || uid != 7 {
		t.Fatalf("解析 uid 失败: %d %v", uid, ok)
	}

	// 旋转：旧令牌失效。
	plain2, err := rs.Rotate(ctx, 7, plain)
	if err != nil {
		t.Fatal(err)
	}
	if plain2 == plain {
		t.Fatal("旋转后令牌应不同")
	}
	if _, err := rs.Rotate(ctx, 7, plain); err == nil {
		t.Fatal("旧令牌旋转应失败")
	}
	// 吊销后全部失效。
	if err := rs.Revoke(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Rotate(ctx, 7, plain2); err == nil {
		t.Fatal("吊销后旋转应失败")
	}
}

func TestRefreshMatch(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	rs := NewRefreshStore(mem)

	plain, err := rs.IssueWithUID(ctx, 7)
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
	if err := rs.Revoke(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if rs.Match(ctx, 7, plain) {
		t.Fatal("吊销后不应匹配")
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
