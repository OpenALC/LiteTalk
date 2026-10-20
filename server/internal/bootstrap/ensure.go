package bootstrap

import (
	"context"
	"log"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// Ensure 首启打印管理员邀请码；幂等，满足任一条件即跳过：
//   - 码已消费（KV）；
//   - 本次部署已打印过（KV）；
//   - 库内已有用户或服务器（HasUsers / HasServers）——实例已在用，不再自动打印，
//     防止 MemKV 重启丢标记后把已作废的码又打到日志里。
//     （旧探测 UserByID(1) 依赖自增 ID=1，Snowflake 后永不命中，#31 换掉。）
//
// 丢失可执行 -print-admin-invite 重打（PrintAdminInvite）。
func Ensure(ctx context.Context, kv store.KV, st store.Store, jwtSecret string, lg *log.Logger) {
	if kv != nil {
		if _, err := kv.Get(ctx, keyConsumed); err == nil {
			return
		}
		if _, err := kv.Get(ctx, keyPrinted); err == nil {
			return
		}
	}
	if st != nil {
		if ok, err := st.HasUsers(ctx); err == nil && ok {
			return
		}
		if ok, err := st.HasServers(ctx); err == nil && ok {
			return
		}
	}
	printInvite(lg, Code(jwtSecret))
	if kv != nil {
		_ = kv.Set(ctx, keyPrinted, "1", 0)
	}
}

// PrintAdminInvite 重新打印管理员邀请码（-print-admin-invite）。
// 已消费（KV 或库内已有服务器）则只报状态不再亮码——码是一次性的，
// 重打已作废的码只会误导操作员。
func PrintAdminInvite(ctx context.Context, kv store.KV, st store.Store, jwtSecret string, lg *log.Logger) {
	if kv != nil {
		if uid, err := kv.Get(ctx, keyConsumed); err == nil {
			lg.Printf("管理员邀请码已消费（uid=%s），实例已被认领，无需再发。", uid)
			return
		}
	}
	if st != nil {
		if ok, err := st.HasServers(ctx); err == nil && ok {
			lg.Println("实例已被认领（库内已有服务器），管理员邀请码已作废，无需再发。")
			return
		}
	}
	printInvite(lg, Code(jwtSecret))
	if kv != nil {
		_ = kv.Set(ctx, keyPrinted, "1", 0)
	}
}

// printInvite 终端输出形态（§0.8）：码 + 一行用途 + 找回方式。
func printInvite(lg *log.Logger, code string) {
	lg.Println("---------- 管理员邀请码 ----------")
	lg.Printf("管理员邀请码: %s", code)
	lg.Println("在 LiteTalk 客户端「注册」页填入该码，即可创建你的服务器并成为其所有者。")
	lg.Println("丢失可执行: litetalk-server --print-admin-invite")
	lg.Println("----------------------------------")
}
