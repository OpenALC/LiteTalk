// Package bootstrap 首启管理员邀请码（#27）。
// 语义见批次4任务流程单 §0.8 与裁定 2：码由 JWT secret 派生（跨重启稳定），
// 注册时带码者成为默认服务器 owner 并作废该码；不做 users.is_admin（免 0005 迁移）。
//
// 码不落库——HMAC 派生保证跨重启稳定；KV 只存「已消费」「已打印」标记。
// 注意：默认 KV 是进程内 MemKV，不跨重启——重启后消费标记丢失，派生码可再次
// 使用（影响仅限自托管 UX：任何注册用户本来就能建服务器）。Redis 部署下标记跨重启。
package bootstrap

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

const (
	// keyConsumed 消费标记：存在 = 邀请码已用（value 为使用方 uid）。
	keyConsumed = "bootstrap:consumed"
	// keyPrinted 打印标记：存在 = 首启已打印过邀请码，不再重复打印。
	keyPrinted = "bootstrap:printed"
	// codeMsg HMAC 消息域；改动会使全体旧码失效。
	codeMsg = "litetalk:bootstrap:v1"
	// devSecret 与 auth.NewJWT 的空密钥回落值一致：派生码与令牌始终同源。
	devSecret = "litetalk-dev-secret-change-me"
	// DefaultServerName 注册引导创建的默认服务器名。
	DefaultServerName = "默认服务器"
)

// Code 从 JWT secret 派生 32 hex 管理员邀请码。
// 同一 secret 恒得同一码（跨重启稳定）；secret 变更则旧码作废。
func Code(jwtSecret string) string {
	if jwtSecret == "" {
		jwtSecret = devSecret
	}
	mac := hmac.New(sha256.New, []byte(jwtSecret))
	mac.Write([]byte(codeMsg))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// Verify 校验邀请码（常数时间比较，容忍前后空白）。
func Verify(jwtSecret, code string) bool {
	want := Code(jwtSecret)
	got := strings.TrimSpace(code)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Required 管理员邀请码是否尚未被消费（实例未被认领）。
// GET /api/v1/config 的 bootstrap_required 据此返回。
// 读失败按未消费处理——提示性字段 fail-open，Redis 闪断不应把注册页锁死。
func Required(ctx context.Context, kv store.KV) bool {
	if kv == nil {
		return false
	}
	_, err := kv.Get(ctx, keyConsumed)
	return err != nil
}

// Consume 标记邀请码已消费（value 记使用方 uid 便于排查）。ttl 0 = 不过期。
func Consume(ctx context.Context, kv store.KV, uid int64) error {
	if kv == nil {
		return nil
	}
	return kv.Set(ctx, keyConsumed, strconv.FormatInt(uid, 10), 0)
}
