// Package bootstrap 首启管理员邀请码（#27，T4 填实现）。
// T3（#26）只预留调用点与签名，保证 T4 不必再动 main.go 主流程：
// 启动序步骤 8 调 Ensure（幂等生成并打印）；-print-admin-invite 调 PrintAdminInvite 后退出。
// 语义见批次4任务流程单 §0.8 与裁定 2：码由 JWT secret 派生（跨重启稳定），
// 注册时带码者成为默认服务器 owner，消费后作废。
package bootstrap

import (
	"context"
	"log"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// Ensure 首启生成管理员邀请码并打印到终端；幂等（已生成不重复）。
// T4（#27）填实现：KV 标记 bootstrap:generated + 码从 jwtSecret 派生。
func Ensure(ctx context.Context, kv store.KV, st store.Store, jwtSecret string, lg *log.Logger) {
	// 桩：T4 填充。当前无行为，不打印（避免误导操作员）。
	_, _, _, _, _ = ctx, kv, st, jwtSecret, lg
}

// PrintAdminInvite 重新打印管理员邀请码（-print-admin-invite）。
// T4（#27）填实现：打印已存在的码（无则生成并打印）。
func PrintAdminInvite(ctx context.Context, kv store.KV, st store.Store, jwtSecret string, lg *log.Logger) {
	_, _, _, _, _ = ctx, kv, st, jwtSecret, lg
	lg.Println("管理员邀请码功能待启用（#27）")
}

// Required 管理员邀请码是否尚未被消费（实例未被领养）。
// T4（#27）填实现：读 KV 消费标记；GET /api/v1/config 的 bootstrap_required 据此返回。
func Required(ctx context.Context, kv store.KV) bool {
	_ = ctx
	_ = kv
	return false
}
