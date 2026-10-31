package store

import (
	"context"
	"testing"
)

// testBotUsers 建一个普通用户与一个 bot 用户，返回 (普通 uid, bot uid)。
func testBotUsers(t *testing.T, st Store, ctx context.Context) (int64, int64) {
	t.Helper()
	human := &User{Email: "human@x", Nickname: "human", Tag: "0001", PasswordHash: "x"}
	bot := &User{Email: "b1@bots.invalid", Nickname: "bot", Tag: "0002", IsBot: true}
	if err := st.CreateUser(ctx, human); err != nil {
		t.Fatalf("CreateUser human: %v", err)
	}
	if err := st.CreateUser(ctx, bot); err != nil {
		t.Fatalf("CreateUser bot: %v", err)
	}
	return human.ID, bot.ID
}

// assertBotsAmong 两个后端共用的断言体：bot 命中、普通用户不命中、不存在的 id
// 忽略、空入参空结果、UserByID 往返一致。
func assertBotsAmong(t *testing.T, st Store, ctx context.Context) {
	t.Helper()
	human, bot := testBotUsers(t, st, ctx)
	got, err := st.BotsAmong(ctx, []int64{human, bot, 999999})
	if err != nil {
		t.Fatalf("BotsAmong: %v", err)
	}
	if len(got) != 1 || !got[bot] {
		t.Fatalf("应只命中 bot %d: %v", bot, got)
	}
	empty, err := st.BotsAmong(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应为空结果: %v %v", empty, err)
	}
	// bot 账号 = users.is_bot=1 的唯一判据，回读必须往返一致。
	u, err := st.UserByID(ctx, bot)
	if err != nil || !u.IsBot {
		t.Fatalf("UserByID 应回读 is_bot=true: %v %v", u, err)
	}
	hu, err := st.UserByID(ctx, human)
	if err != nil || hu.IsBot {
		t.Fatalf("普通用户 is_bot 应为 false: %v %v", hu, err)
	}
}

// TestBotsAmong 内存与 SQLite 后端（无条件跑，外部服务零依赖）。
func TestBotsAmong(t *testing.T) {
	ctx := context.Background()

	t.Run("mem", func(t *testing.T) {
		assertBotsAmong(t, NewMem(), ctx)
	})
	t.Run("sqlite", func(t *testing.T) {
		sq := openTestSQLite(t)
		if err := sq.Migrate(ctx, findMigrationsSQLite(t)); err != nil {
			t.Fatalf("SQLite 迁移: %v", err)
		}
		assertBotsAmong(t, sq, ctx)
	})
}

// TestBotsAmongMySQL 真库矩阵（MySQL 不可用自动跳过，与既有真库测试同口径）。
func TestBotsAmongMySQL(t *testing.T) {
	m := openTestMySQL(t)
	defer m.Close()
	ctx := context.Background()
	if _, err := m.DB().ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS litetalk_test"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx, "USE litetalk_test"); err != nil {
		t.Fatal(err)
	}
	// 清理历史：必须覆盖每条迁移建过的表（同 TestMySQLMigrateAndCRUD 清单），
	// 否则上一轮测试留下的表会让 CREATE TABLE 直接 1050。
	for _, tbl := range []string{
		"attachments", "read_states", "invites", "messages", "message_mentions", "channel_members", "channels", "categories", "server_members",
		"servers", "friendships", "users", "user_sessions", "kv", "schema_migrations",
		"audit_log", "channel_overwrites", "member_roles", "roles", "api_tokens",
		"push_devices",
		"webhook_deliveries", "inbound_webhooks", "webhooks",
	} {
		_, _ = m.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}
	if err := m.Migrate(ctx, findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	assertBotsAmong(t, m, ctx)
}
