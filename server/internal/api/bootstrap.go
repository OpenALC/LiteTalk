// 注册侧 bootstrap 邀请码接线（#27）：校验 → 认领默认服务器 → 消费标记。
// 派生/标记逻辑在 internal/bootstrap；这里只做 HTTP 流程胶水。
// 依赖获取不改 main.go 接线（裁定 13）：
//   - 码的派生密钥从 s.jwt.Secret() 取（与令牌同源）；
//   - 消费标记写 KV——main 把同一个 kv 作为 Presence 传入，此处类型断言取回。
package api

import (
	"context"
	"errors"

	"github.com/OpenALC/LiteTalk/server/internal/bootstrap"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// bootstrapKV 消费标记存储。测试传入只读 fake 时取不到 → 带码注册直接拒
// （fail-closed：不能消费的码不该发出去）。生产 main 恒传 store.KV。
func (s *Server) bootstrapKV() store.KV {
	kv, _ := s.presence.(store.KV)
	return kv
}

// checkBootstrap 邀请码前置校验（建用户之前）：码合法且未消费。
// 不合法/已用 → errBadBootstrap，调用方回 400 bad_bootstrap。
func (s *Server) checkBootstrap(ctx context.Context, code string) error {
	if !bootstrap.Verify(string(s.jwt.Secret()), code) {
		return errBadBootstrap
	}
	kv := s.bootstrapKV()
	if kv == nil || !bootstrap.Required(ctx, kv) {
		return errBadBootstrap
	}
	return nil
}

// claimBootstrap 注册成功后认领：建默认服务器并标记码已消费。
// 消费标记写失败只记日志——用户与服务器都已建好，重发 500 反而让客户端
// 误以为注册失败；代价是该码在标记落上之前仍可再用（自托管一次性 UX，可接受）。
func (s *Server) claimBootstrap(ctx context.Context, code string, user *store.User) {
	if !bootstrap.Verify(string(s.jwt.Secret()), code) {
		return // 前置 checkBootstrap 已拒；防御不重复执行
	}
	if _, err := bootstrap.Adopt(ctx, s.store, user.ID); err != nil {
		s.log.Printf("[bootstrap] 建默认服务器失败 uid=%d: %v", user.ID, err)
		return
	}
	if kv := s.bootstrapKV(); kv != nil {
		if err := bootstrap.Consume(ctx, kv, user.ID); err != nil {
			s.log.Printf("[bootstrap] 消费标记写入失败 uid=%d: %v", user.ID, err)
		}
	}
}

// errBadBootstrap 前置校验失败（码不合法或已用）。
var errBadBootstrap = errors.New("bad_bootstrap")
