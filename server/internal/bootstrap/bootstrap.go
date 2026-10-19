// Package bootstrap 首启管理员邀请码（#27）。
// 语义见批次4任务流程单 §0.8 与裁定 2：码由 JWT secret 派生（跨重启稳定），
// 注册时带码者成为默认服务器 owner 并作废该码；不做 users.is_admin（免 0005 迁移）。
//
// 码不落库——HMAC 派生保证跨重启稳定；KV 只存「已消费」「已打印」标记。
// 「实例是否已被认领」以库内是否存在服务器为准（HasServers）：默认 KV 是进程内
// MemKV，重启丢标记，只看 KV 会让同一个码再注册出一个默认服务器。
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

// Required 实例是否仍等待管理员邀请码认领。
// 判据：KV 已消费 → false；库内已有服务器（实例已被认领）→ false；其余 → true。
// GET /api/v1/config 的 bootstrap_required 与注册侧校验都据此返回（#27）。
// kv == nil 视为流程未启用（无法写消费标记，注册侧同样 fail-closed）。
// 存储读失败按「未认领」处理——提示性字段 fail-open，Redis 闪断不应把注册页锁死。
func Required(ctx context.Context, kv store.KV, st store.Store) bool {
	if kv == nil {
		return false
	}
	if _, err := kv.Get(ctx, keyConsumed); err == nil {
		return false
	}
	if st != nil {
		if ok, err := st.HasServers(ctx); err == nil && ok {
			return false
		}
	}
	return true
}

// Consume 标记邀请码已消费（value 记使用方 uid 便于排查）。ttl 0 = 不过期。
func Consume(ctx context.Context, kv store.KV, uid int64) error {
	if kv == nil {
		return nil
	}
	return kv.Set(ctx, keyConsumed, strconv.FormatInt(uid, 10), 0)
}
