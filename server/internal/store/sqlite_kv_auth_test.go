package store_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/bootstrap"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/migrations"
)

// openKVSQLite 打开（必要时创建/迁移）指定库文件——用同一个路径重开即模拟服务重启。
func openKVSQLite(t *testing.T, path string) *store.SQLite {
	t.Helper()
	t.Setenv("LITETALK_SQLITE_PATH", "") // 防止环境变量覆盖 TempDir 路径
	sq, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(%s): %v", path, err)
	}
	sub, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		t.Fatalf("fs.Sub sqlite: %v", err)
	}
	if err := sq.Migrate(context.Background(), sub); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	return sq
}

// TestSQLiteKVRefreshTokenSurvivesRestart #83 验收：同一 SQLite 文件重启后，
// 旧刷新令牌仍能旋转出新令牌（游客只靠刷新令牌维持身份，走同一条路径）。
func TestSQLiteKVRefreshTokenSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "litetalk.db")

	sq := openKVSQLite(t, path)
	rs := auth.NewRefreshStore(store.NewSQLiteKV(sq.DB()))
	plain, err := rs.IssueWithUID(ctx, 7, "desktop", "sid-1")
	if err != nil {
		t.Fatalf("IssueWithUID: %v", err)
	}
	if err := sq.Close(); err != nil {
		t.Fatalf("关闭模拟重启: %v", err)
	}

	sq2 := openKVSQLite(t, path)
	defer sq2.Close()
	rs2 := auth.NewRefreshStore(store.NewSQLiteKV(sq2.DB()))

	rotated, err := rs2.Rotate(ctx, 7, plain)
	if err != nil {
		t.Fatalf("重启后旧刷新令牌应仍可旋转: %v", err)
	}
	if rotated == "" {
		t.Fatal("旋转后应返回新令牌")
	}
	// 旧令牌重放必须失败，证明旋转结果也落库了。
	if _, err := rs2.Rotate(ctx, 7, plain); !errors.Is(err, auth.ErrSessionReplayed) {
		t.Fatalf("旧令牌重放应 ErrSessionReplayed，实际 %v", err)
	}
}

// TestSQLiteKVBootstrapMarkerSurvivesRestart #83/#27 验收：引导邀请码的已消费
// 标记跨重启保留，实例不会被同一个码再次认领。
func TestSQLiteKVBootstrapMarkerSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "litetalk.db")
	st := store.NewMem() // 库内没有服务器：判定只能依赖 KV 标记

	sq := openKVSQLite(t, path)
	if err := bootstrap.Consume(ctx, store.NewSQLiteKV(sq.DB()), 9); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := sq.Close(); err != nil {
		t.Fatalf("关闭模拟重启: %v", err)
	}

	sq2 := openKVSQLite(t, path)
	defer sq2.Close()
	kv := store.NewSQLiteKV(sq2.DB())
	if bootstrap.Required(ctx, kv, st) {
		t.Fatal("重启后消费标记仍在，不应再要求管理员邀请码")
	}
	// 对照：进程内 KV 重启后标记丢失，判定回落到 true（证明上一条断言不是恒真）。
	if !bootstrap.Required(ctx, store.NewMemKV(), st) {
		t.Fatal("MemKV 重启后应丢失消费标记（对照）")
	}
}
