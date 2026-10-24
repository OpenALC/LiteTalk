package store

import (
	"context"
	"testing"
	"time"
)

// #42 B1：迁移 0016 的 up/down/再 up，以及 SQLite 应用层索引同步。

// insertLegacyMessage 在 0016 之前直接写 messages（此时还没有 FTS 虚表/索引，
// store.CreateMessage 会因索引表不存在而失败）。
func insertLegacyMessage(t *testing.T, m *SQLite, id, channelID, userID int64, content string) {
	t.Helper()
	if _, err := m.DB().ExecContext(context.Background(),
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (?, ?, ?, ?)`,
		id, channelID, userID, content); err != nil {
		t.Fatalf("预置消息 %d 失败: %v", id, err)
	}
}

func ftsTableExists(t *testing.T, m *SQLite) bool {
	t.Helper()
	var n int
	if err := m.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts'`).Scan(&n); err != nil {
		t.Fatalf("查询 messages_fts: %v", err)
	}
	return n > 0
}

func TestSearchMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	before := migrationsBefore(t, full, "0016")
	through := migrationsBefore(t, full, "0017")
	if err := m.Migrate(ctx, before); err != nil {
		t.Fatalf("迁移到 0015 失败: %v", err)
	}
	if ftsTableExists(t, m) {
		t.Fatal("0016 之前不应存在 messages_fts")
	}
	u := &User{Email: "mig-search@ex.com", Nickname: "迁移搜索", Tag: "0001"}
	if err := m.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const chID = int64(7001)
	insertLegacyMessage(t, m, 6001, chID, u.ID, "升级前关键词")

	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("应用 0016 失败: %v", err)
	}
	if !ftsTableExists(t, m) {
		t.Fatal("up 后 messages_fts 应存在")
	}
	// 回填：升级前已存在的消息可搜。
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级前关键词", 20), 6001)

	// 新插入 → 索引同步。
	created := &Message{ChannelID: chID, UserID: u.ID, Content: "升级后新增词"}
	if err := m.CreateMessage(ctx, created); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级后新增", 20), created.ID)

	// 编辑 → 旧词移除、新词可搜。
	if err := m.UpdateMessageContent(ctx, 6001, "升级前关键词已改", nil, time.Now()); err != nil {
		t.Fatalf("UpdateMessageContent: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级前关键词已改", 20), 6001)
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级前关键词", 20), 6001) // 新正文包含旧词，合理命中
	if err := m.UpdateMessageContent(ctx, 6001, "完全换了正文", nil, time.Now()); err != nil {
		t.Fatalf("UpdateMessageContent(2): %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级前关键词", 20))
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "完全换了正文", 20), 6001)

	// 软删 → 索引项移除。
	if err := m.SoftDeleteMessage(ctx, created.ID, time.Now()); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级后新增", 20))

	// down：虚表消失。
	if err := m.RollbackLast(ctx, through); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	if ftsTableExists(t, m) {
		t.Fatal("down 后 messages_fts 应消失")
	}
	// 再 up：从当前 messages 重新回填。
	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "完全换了正文", 20), 6001)
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级后新增", 20))
}

// TestSearchMigrationMySQL 0016 up/down/再 up（DSN 不可用时跳过）。
// ngram 是 MySQL 内置 parser：5.7+/8.0+ 原生支持；MariaDB 需要 10.10+，
// 版本不足时本测试会在 ALTER TABLE 处失败并带上实际错误，不会静默跳过。
func TestSearchMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	var version string
	if err := m.DB().QueryRowContext(ctx, `SELECT VERSION()`).Scan(&version); err != nil {
		t.Fatalf("查询 MySQL 版本失败: %v", err)
	}
	t.Logf("MySQL 版本：%s", version)

	full := findMigrations(t)
	before := migrationsBefore(t, full, "0016")
	through := migrationsBefore(t, full, "0017")
	if err := m.Migrate(ctx, before); err != nil {
		t.Fatalf("迁移到 0015 失败: %v", err)
	}
	// 0016 之前 MATCH 应因缺 FULLTEXT 索引报错（错误码 1191）。
	if _, err := m.DB().ExecContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE MATCH(content) AGAINST('旧词' IN NATURAL LANGUAGE MODE)`); err == nil {
		t.Fatal("0016 之前 MATCH 应失败（无 FULLTEXT 索引）")
	}
	// 用户必须先真实存在：SearchMessages 按 users 表内联发送者昵称/tag。
	u := &User{Email: "mig-search@ex.com", Nickname: "迁移搜索", Tag: "0001"}
	if err := m.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const chID = int64(7001)
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (6001, ?, ?, '升级前关键词')`,
		chID, u.ID); err != nil {
		t.Fatalf("预置消息失败: %v", err)
	}

	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("应用 0016 失败（ngram 是否可用？实际版本 %s）: %v", version, err)
	}
	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.statistics
		  WHERE table_schema = DATABASE() AND table_name = 'messages'
		    AND index_name = 'ft_messages_content'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("up 后 ft_messages_content 应存在: %d %v", n, err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE MATCH(content) AGAINST(? IN NATURAL LANGUAGE MODE)`,
		"升级前关键词").Scan(&n); err != nil || n != 1 {
		t.Fatalf("回填后 MATCH 应命中 1 条: %d %v", n, err)
	}

	// store 层：新插入/编辑/软删由 InnoDB 自动维护全文索引。
	created := &Message{ChannelID: chID, UserID: u.ID, Content: "升级后新增词"}
	if err := m.CreateMessage(ctx, created); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级后新增", 20), created.ID)
	if err := m.UpdateMessageContent(ctx, 6001, "完全换了正文", nil, time.Now()); err != nil {
		t.Fatalf("UpdateMessageContent: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级前关键词", 20))
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "完全换了正文", 20), 6001)
	if err := m.SoftDeleteMessage(ctx, created.ID, time.Now()); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "升级后新增", 20))

	// down：索引删除。
	if err := m.RollbackLast(ctx, through); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.statistics
		  WHERE table_schema = DATABASE() AND table_name = 'messages'
		    AND index_name = 'ft_messages_content'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down 后 ft_messages_content 应消失: %d %v", n, err)
	}
	// 再 up：索引重建，仍可搜到未删除的消息。
	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, m, []int64{chID}, "完全换了正文", 20), 6001)
}
