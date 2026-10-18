package auth

import (
	"context"
	"errors"
	"time"
)

// 验证码策略（§8）：6 位 / 10min 有效 / 错误 5 次作废 / 60s 重发间隔 / 同邮箱 5 次每日。
const (
	CodeTTL        = 10 * time.Minute
	CodeAttempts   = 5
	CodeInterval   = 60 * time.Second
	CodeDailyLimit = 5
)

// 用途常量。
const (
	PurposeRegister = "register"
	PurposeReset    = "reset"
)

// ErrCodeExpired 验证码不存在或已过期。
var ErrCodeExpired = errors.New("code expired")

// ErrCodeInvalid 验证码错误（含超次作废）。
var ErrCodeInvalid = errors.New("code invalid")

// ErrCodeTooFrequent 重发过快。
var ErrCodeTooFrequent = errors.New("code too frequent")

// ErrCodeDailyLimit 当日发送超限。
var ErrCodeDailyLimit = errors.New("code daily limit")

// EmailCodes 邮箱验证码管理（基于 KV 抽象）。
type EmailCodes struct {
	kv kvStore
}

// NewEmailCodes 构造管理器。
func NewEmailCodes(kv kvStore) *EmailCodes { return &EmailCodes{kv: kv} }

func codeKey(purpose, email string) string  { return "code:" + purpose + ":" + email }
func triesKey(purpose, email string) string { return "code:tries:" + purpose + ":" + email }
func intervalKey(purpose, email string) string {
	return "rl:code:interval:" + purpose + ":" + email
}
func dailyKey(purpose, email string) string { return "rl:code:daily:" + purpose + ":" + email }

// Send 生成并存储验证码。返回明文验证码（由 mailer 发出）。
// 频控：60s 间隔 + 同用途同邮箱每日 5 次。
func (e *EmailCodes) Send(ctx context.Context, purpose, email string) (string, error) {
	// 60s 间隔检查。
	if _, err := e.kv.Get(ctx, intervalKey(purpose, email)); err == nil {
		return "", ErrCodeTooFrequent
	}
	// 每日限次。
	n, err := e.kv.Incr(ctx, dailyKey(purpose, email), 24*time.Hour)
	if err != nil {
		return "", err
	}
	if n > CodeDailyLimit {
		return "", ErrCodeDailyLimit
	}

	code, err := RandomCode()
	if err != nil {
		return "", err
	}
	if err := e.kv.Set(ctx, codeKey(purpose, email), code, CodeTTL); err != nil {
		return "", err
	}
	// 重置错误计数。
	_ = e.kv.Del(ctx, triesKey(purpose, email))
	// 标记重发间隔。
	if err := e.kv.Set(ctx, intervalKey(purpose, email), "1", CodeInterval); err != nil {
		return "", err
	}
	return code, nil
}

// Verify 校验验证码。成功后立即销毁，防重放；错误累计 5 次作废重发。
func (e *EmailCodes) Verify(ctx context.Context, purpose, email, code string) error {
	stored, err := e.kv.Get(ctx, codeKey(purpose, email))
	if err != nil {
		return ErrCodeExpired
	}
	if stored != code {
		n, _ := e.kv.Incr(ctx, triesKey(purpose, email), CodeTTL)
		if n >= CodeAttempts {
			// 超次作废。
			_ = e.kv.Del(ctx, codeKey(purpose, email), triesKey(purpose, email))
		}
		return ErrCodeInvalid
	}
	// 验证通过即销毁。
	_ = e.kv.Del(ctx, codeKey(purpose, email), triesKey(purpose, email))
	return nil
}

// ---- 通用限流（登录 5 次/min/IP）----

// RateLimit 固定窗口限流器。
type RateLimit struct {
	kv kvStore
}

// NewRateLimit 构造限流器。
func NewRateLimit(kv kvStore) *RateLimit { return &RateLimit{kv: kv} }

// Allow 窗口内第 n 次请求；超过 limit 返回 false。计数走 IncrCap 原子限幅（#22）。
func (r *RateLimit) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	ok, _, _, err := r.AllowInfo(ctx, key, limit, window)
	return ok, err
}

// AllowInfo 与 Allow 同判定，另返回剩余配额与窗口重置上限（#31 限流头）。
// reset 是「此刻 + 窗口长」的上界（固定窗口起点不可回查，重置只会更早不会更晚），
// 调用方可直接写 X-RateLimit-Reset 与 Retry-After，客户端按它退避只会多等不会少等。
func (r *RateLimit) AllowInfo(ctx context.Context, key string, limit int, window time.Duration) (bool, int, time.Time, error) {
	n, err := r.kv.IncrCap(ctx, "rl:"+key, window, int64(limit))
	if err != nil {
		return false, 0, time.Time{}, err
	}
	remaining := int(int64(limit) - n)
	if remaining < 0 {
		remaining = 0
	}
	return n <= int64(limit), remaining, time.Now().Add(window), nil
}
