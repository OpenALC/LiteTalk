package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// #39 B1：消息编辑/软删除/回复/@提及（迁移 0013）。本文件覆盖双方言迁移与
// MySQL/SQLite/Mem 三实现契约。

// messageModelSeq 保证同一二进制内用户 email/nickname 唯一（Mem 与真库用例共用）。
var messageModelSeq atomic.Int64

// messageModelUsers 建 n 个互相独立的用户（每次调用一组新数据）。
func messageModelUsers(t *testing.T, st Store, n int) []*User {
	t.Helper()
	ctx := context.Background()
	seq := messageModelSeq.Add(1)
	out := make([]*User, n)
	for i := range out {
		u := &User{
			Email:        fmt.Sprintf("msg-%d-%d@ex.test", seq, i),
			Nickname:     fmt.Sprintf("mm%d-%d", seq%100000, i),
			Tag:          "0001",
			PasswordHash: "h",
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser(%d): %v", i, err)
		}
		out[i] = u
	}
	return out
}

// messageModelChannel 在服务器下建一个文字频道（每个子用例独立频道，互不干扰）。
func messageModelChannel(t *testing.T, st Store, serverID int64, name string) *Channel {
	t.Helper()
	ch := &Channel{ServerID: serverID, Name: name, Type: ChannelText}
	if err := st.CreateChannel(context.Background(), ch); err != nil {
		t.Fatalf("CreateChannel(%s): %v", name, err)
	}
	return ch
}

// wantMentionIDs 断言提及列表按 user_id 升序且内容完全一致（空/缺省统一为 nil）。
func wantMentionIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !slices.Equal(got, want) {
		t.Fatalf("提及 = %v，期望 %v", got, want)
	}
}

// runMessageModelContract MySQL/SQLite/Mem 共用契约。
func runMessageModelContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	us := messageModelUsers(t, st, 3)
	owner, other, third := us[0], us[1], us[2]
	sv := &Server{Name: "msg-model", OwnerID: owner.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	t.Run("创建回复与提及", func(t *testing.T) {
		ch := messageModelChannel(t, st, sv.ID, "reply")
		orig := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "原始",
			// 乱序 + 重复，Store 必须去重并按 user_id 升序落库。
			Mentions: []int64{third.ID, other.ID, third.ID},
		}
		if err := st.CreateMessage(ctx, orig); err != nil {
			t.Fatalf("CreateMessage(orig): %v", err)
		}
		reply := &Message{
			ChannelID: ch.ID, UserID: other.ID, Content: "回复",
			ReplyTo: &orig.ID, Mentions: []int64{owner.ID},
		}
		if err := st.CreateMessage(ctx, reply); err != nil {
			t.Fatalf("CreateMessage(reply): %v", err)
		}

		got, err := st.MessageByID(ctx, reply.ID)
		if err != nil {
			t.Fatalf("MessageByID(reply): %v", err)
		}
		if got.ReplyTo == nil || *got.ReplyTo != orig.ID {
			t.Fatalf("reply_to 往返失真: %+v", got.ReplyTo)
		}
		if got.EditedAt != nil || got.DeletedAt != nil {
			t.Fatalf("新消息不应有编辑/删除态: %+v", got)
		}
		if got.SenderNickname != other.Nickname || got.SenderTag != other.Tag {
			t.Fatalf("MessageByID 应内联发送者: %+v", got)
		}
		if _, err := st.MessageByID(ctx, 999999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("未命中应 ErrNotFound，实际 %v", err)
		}

		// 分页扫描新列：原消息 reply_to 为 NULL，回复行带值。
		list, err := st.MessagesBefore(ctx, ch.ID, 0, 50)
		if err != nil || len(list) != 2 {
			t.Fatalf("MessagesBefore: %d %v", len(list), err)
		}
		if list[0].ID != orig.ID || list[0].ReplyTo != nil || list[0].EditedAt != nil || list[0].DeletedAt != nil {
			t.Fatalf("原消息新列应全 NULL: %+v", list[0])
		}
		if list[1].ReplyTo == nil || *list[1].ReplyTo != orig.ID {
			t.Fatalf("列表扫描丢失 reply_to: %+v", list[1])
		}

		mentions, err := st.MentionsOfMessages(ctx, []int64{orig.ID, reply.ID, 999999999})
		if err != nil {
			t.Fatalf("MentionsOfMessages: %v", err)
		}
		if len(mentions) != 2 {
			t.Fatalf("提及只应有两条消息: %+v", mentions)
		}
		wantMentionIDs(t, mentions[orig.ID], other.ID, third.ID)
		wantMentionIDs(t, mentions[reply.ID], owner.ID)
		if empty, err := st.MentionsOfMessages(ctx, nil); err != nil || len(empty) != 0 {
			t.Fatalf("空入参应返回空 map: %+v %v", empty, err)
		}

		batch, err := st.MessagesByIDs(ctx, []int64{orig.ID, reply.ID, 999999999})
		if err != nil {
			t.Fatalf("MessagesByIDs: %v", err)
		}
		if len(batch) != 2 || batch[orig.ID] == nil || batch[reply.ID] == nil {
			t.Fatalf("批量回读不符: %+v", batch)
		}
		if batch[reply.ID].ReplyTo == nil || *batch[reply.ID].ReplyTo != orig.ID || batch[reply.ID].Content != "回复" {
			t.Fatalf("批量回读 reply: %+v", batch[reply.ID])
		}
		if batch[orig.ID].SenderNickname != owner.Nickname {
			t.Fatalf("批量回读应内联发送者: %+v", batch[orig.ID])
		}
		if empty, err := st.MessagesByIDs(ctx, nil); err != nil || len(empty) != 0 {
			t.Fatalf("空入参应返回空 map: %+v %v", empty, err)
		}
	})

	t.Run("nonce 冲突整体回滚", func(t *testing.T) {
		ch := messageModelChannel(t, st, sv.ID, "nonce")
		first := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "一", Nonce: "dup",
			Mentions: []int64{other.ID},
		}
		if err := st.CreateMessage(ctx, first); err != nil {
			t.Fatalf("CreateMessage(first): %v", err)
		}
		dup := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "二", Nonce: "dup",
			Mentions: []int64{third.ID},
		}
		if err := st.CreateMessage(ctx, dup); !errors.Is(err, ErrConflict) {
			t.Fatalf("同 nonce 应 ErrConflict，实际 %v", err)
		}
		// 冲突消息不得留下提及；原消息的提及也未被改动。
		mentions, err := st.MentionsOfMessages(ctx, []int64{first.ID, dup.ID})
		if err != nil {
			t.Fatalf("MentionsOfMessages: %v", err)
		}
		if len(mentions) != 1 {
			t.Fatalf("冲突后应只剩原消息的提及: %+v", mentions)
		}
		wantMentionIDs(t, mentions[first.ID], other.ID)

		list, err := st.MessagesBefore(ctx, ch.ID, 0, 50)
		if err != nil || len(list) != 1 {
			t.Fatalf("冲突不应写入消息: %d %v", len(list), err)
		}
	})

	t.Run("更新正文替换提及", func(t *testing.T) {
		ch := messageModelChannel(t, st, sv.ID, "edit")
		msg := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "原文",
			Mentions: []int64{third.ID},
		}
		if err := st.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
		editedAt := time.Now().UTC().Truncate(time.Second)
		if err := st.UpdateMessageContent(ctx, msg.ID, "改后", []int64{third.ID, owner.ID, third.ID}, editedAt); err != nil {
			t.Fatalf("UpdateMessageContent: %v", err)
		}
		got, err := st.MessageByID(ctx, msg.ID)
		if err != nil || got.Content != "改后" {
			t.Fatalf("更新未生效: %+v %v", got, err)
		}
		if got.EditedAt == nil || !got.EditedAt.Equal(editedAt) {
			t.Fatalf("edited_at 往返失真: got=%v want=%v", got.EditedAt, editedAt)
		}
		mentions, err := st.MentionsOfMessages(ctx, []int64{msg.ID})
		if err != nil {
			t.Fatalf("MentionsOfMessages: %v", err)
		}
		wantMentionIDs(t, mentions[msg.ID], owner.ID, third.ID)

		// 同值更新（正文与 edited_at 都不变）必须成功：MySQL 未开 clientFoundRows
		// 时受影响行数为 0，不能误判为不存在。
		if err := st.UpdateMessageContent(ctx, msg.ID, "改后", nil, editedAt); err != nil {
			t.Fatalf("同值更新应成功: %v", err)
		}
		// 空提及 = 清空该消息的全部提及。
		mentions, err = st.MentionsOfMessages(ctx, []int64{msg.ID})
		if err != nil || len(mentions) != 0 {
			t.Fatalf("清空提及后不应残留: %+v %v", mentions, err)
		}
		if err := st.UpdateMessageContent(ctx, 999999999, "x", nil, editedAt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("软删除保留行", func(t *testing.T) {
		ch := messageModelChannel(t, st, sv.ID, "delete")
		msg := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "要删的",
			Mentions: []int64{other.ID},
		}
		if err := st.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
		deletedAt := time.Now().UTC().Truncate(time.Second)
		if err := st.SoftDeleteMessage(ctx, msg.ID, deletedAt); err != nil {
			t.Fatalf("SoftDeleteMessage: %v", err)
		}
		got, err := st.MessageByID(ctx, msg.ID)
		if err != nil {
			t.Fatalf("软删行应仍可回读: %v", err)
		}
		if got.DeletedAt == nil || !got.DeletedAt.Equal(deletedAt) {
			t.Fatalf("deleted_at 往返失真: got=%v want=%v", got.DeletedAt, deletedAt)
		}
		if got.Content != "" {
			t.Fatalf("软删应清空正文，实际 %q", got.Content)
		}
		// 行保留在分页结果里（供回复引用），只是带删除态、正文为空。
		list, err := st.MessagesBefore(ctx, ch.ID, 0, 50)
		if err != nil || len(list) != 1 {
			t.Fatalf("软删行应保留在列表: %d %v", len(list), err)
		}
		if list[0].DeletedAt == nil || list[0].Content != "" {
			t.Fatalf("列表中的删除态不符: %+v", list[0])
		}
		// 回复软删消息仍能回读引用 id。
		reply := &Message{ChannelID: ch.ID, UserID: other.ID, Content: "回复墓碑", ReplyTo: &msg.ID}
		if err := st.CreateMessage(ctx, reply); err != nil {
			t.Fatalf("CreateMessage(reply): %v", err)
		}
		if got, err := st.MessageByID(ctx, reply.ID); err != nil || got.ReplyTo == nil || *got.ReplyTo != msg.ID {
			t.Fatalf("回复墓碑失真: %+v %v", got, err)
		}
		// 二次删除、编辑已删消息、删除不存在：一律 ErrNotFound。
		if err := st.SoftDeleteMessage(ctx, msg.ID, deletedAt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("重复删除应 ErrNotFound，实际 %v", err)
		}
		if err := st.UpdateMessageContent(ctx, msg.ID, "复活", nil, deletedAt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("编辑已删消息应 ErrNotFound，实际 %v", err)
		}
		if err := st.SoftDeleteMessage(ctx, 999999999, deletedAt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除不存在应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("删除频道清理提及与服务器级联", func(t *testing.T) {
		parent := messageModelChannel(t, st, sv.ID, "parent")
		child := &Channel{ServerID: sv.ID, Name: "child", Type: ChannelText, ParentID: &parent.ID}
		if err := st.CreateChannel(ctx, child); err != nil {
			t.Fatalf("CreateChannel(child): %v", err)
		}
		pm := &Message{
			ChannelID: parent.ID, UserID: owner.ID, Content: "父频道",
			Mentions: []int64{other.ID},
		}
		cm := &Message{
			ChannelID: child.ID, UserID: owner.ID, Content: "子频道",
			Mentions: []int64{third.ID},
		}
		for _, msg := range []*Message{pm, cm} {
			if err := st.CreateMessage(ctx, msg); err != nil {
				t.Fatalf("CreateMessage(%d): %v", msg.ChannelID, err)
			}
		}
		if err := st.DeleteChannel(ctx, parent.ID); err != nil {
			t.Fatalf("DeleteChannel: %v", err)
		}
		mentions, err := st.MentionsOfMessages(ctx, []int64{pm.ID, cm.ID})
		if err != nil {
			t.Fatalf("MentionsOfMessages: %v", err)
		}
		if len(mentions) != 0 {
			t.Fatalf("删频道后提及应清空（父/子频道），实际 %+v", mentions)
		}
		if _, err := st.MessageByID(ctx, pm.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("父频道消息应删除，实际 %v", err)
		}

		// 删服务器同样要清提及（没有外键，漏删会留孤儿行）。
		sv2 := &Server{Name: "msg-model-2", OwnerID: owner.ID, IconName: "hash", IconColor: "#4FA4F3"}
		if err := st.CreateServer(ctx, sv2); err != nil {
			t.Fatalf("CreateServer(2): %v", err)
		}
		ch2 := messageModelChannel(t, st, sv2.ID, "chat2")
		m2 := &Message{
			ChannelID: ch2.ID, UserID: owner.ID, Content: "随服删除",
			Mentions: []int64{other.ID, third.ID},
		}
		if err := st.CreateMessage(ctx, m2); err != nil {
			t.Fatalf("CreateMessage(m2): %v", err)
		}
		if err := st.DeleteServer(ctx, sv2.ID); err != nil {
			t.Fatalf("DeleteServer: %v", err)
		}
		mentions, err = st.MentionsOfMessages(ctx, []int64{m2.ID})
		if err != nil || len(mentions) != 0 {
			t.Fatalf("删服务器后提及应清空: %+v %v", mentions, err)
		}
	})
}

func TestMessageModelContractMem(t *testing.T) {
	runMessageModelContract(t, NewMem())
}

func TestMessageModelContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runMessageModelContract(t, sq)
}

func TestMessageModelContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runMessageModelContract(t, m)
}

// ---- 0013 迁移（双方言） ----

// seedLegacy0013 预置 0012 及以前的形状：两用户/一服/一文字频道/一条消息。
func seedLegacy0013(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES
		 (3101, 'msg3101@ex.test', 'msg3101', '0001', 'h'),
		 (3102, 'msg3102@ex.test', 'msg3102', '0002', 'h')`,
		`INSERT INTO servers (id, name, owner_id) VALUES (3201, 'legacy-msg', 3101)`,
		`INSERT INTO channels (id, server_id, name, type, position) VALUES (3301, 3201, 'text', 'text', 0)`,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (3501, 3301, 3101, 'legacy')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("预置失败: %v", err)
		}
	}
}

// assertLegacyMessage0013 up 后旧消息三列必须为 NULL、正文不变。
func assertLegacyMessage0013(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var editedAt, deletedAt sql.NullTime
	var replyTo sql.NullInt64
	var content string
	if err := db.QueryRowContext(ctx,
		`SELECT content, edited_at, deleted_at, reply_to FROM messages WHERE id = 3501`).
		Scan(&content, &editedAt, &deletedAt, &replyTo); err != nil {
		t.Fatalf("旧消息读回失败: %v", err)
	}
	if content != "legacy" {
		t.Fatalf("旧消息正文漂移: %q", content)
	}
	if editedAt.Valid || deletedAt.Valid || replyTo.Valid {
		t.Fatalf("旧消息新列应全 NULL: edited=%v deleted=%v reply=%v", editedAt, deletedAt, replyTo)
	}
}

// assertLegacyMessageContent0013 down 后新列已不存在，只能断言旧消息行仍在、正文不变。
func assertLegacyMessageContent0013(t *testing.T, db *sql.DB) {
	t.Helper()
	var content string
	if err := db.QueryRowContext(context.Background(),
		`SELECT content FROM messages WHERE id = 3501`).Scan(&content); err != nil {
		t.Fatalf("down 后旧消息应保留: %v", err)
	}
	if content != "legacy" {
		t.Fatalf("down 后旧消息正文漂移: %q", content)
	}
}

// insertProbe0013 写入一条回复 + 两条提及，验证新列/新表可用、主键去重。
func insertProbe0013(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content, reply_to)
		 VALUES (3502, 3301, 3102, 'reply', 3501)`); err != nil {
		t.Fatalf("写回复消息失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO message_mentions (message_id, user_id) VALUES (3502, 3102), (3502, 3101)`); err != nil {
		t.Fatalf("写提及失败: %v", err)
	}
	var replyTo sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT reply_to FROM messages WHERE id = 3502`).Scan(&replyTo); err != nil ||
		!replyTo.Valid || replyTo.Int64 != 3501 {
		t.Fatalf("reply_to 读回不符: %v %v", replyTo, err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_mentions WHERE message_id = 3502`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("提及行数 = %d %v，期望 2", n, err)
	}
	// 复合主键去重。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO message_mentions (message_id, user_id) VALUES (3502, 3101)`); err == nil {
		t.Fatal("重复提及应被主键拒绝")
	}
}

// TestMessageModelMigrationSQLite 0013 up/down/up（SQLite 无条件跑）。
func TestMessageModelMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	until0013 := migrationsBefore(t, full, "0014")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0013")); err != nil {
		t.Fatalf("迁移到 0012 失败: %v", err)
	}
	seedLegacy0013(t, m.DB())
	if err := m.Migrate(ctx, until0013); err != nil {
		t.Fatalf("应用 0013 失败: %v", err)
	}
	assertLegacyMessage0013(t, m.DB())
	insertProbe0013(t, m.DB())

	var cols int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('messages')
		  WHERE name IN ('edited_at','deleted_at','reply_to')`).Scan(&cols); err != nil || cols != 3 {
		t.Fatalf("up 后消息新列应存在: %d %v", cols, err)
	}
	for _, idx := range []string{"idx_messages_reply_to", "idx_message_mentions_user"} {
		var n int
		if err := m.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil || n != 1 {
			t.Fatalf("up 后索引 %s 应存在: %d %v", idx, n, err)
		}
	}
	var tables int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='message_mentions'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("up 后 message_mentions 应存在: %d %v", tables, err)
	}

	if err := m.RollbackLast(ctx, until0013); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	assertLegacyMessageContent0013(t, m.DB())
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('messages')
		  WHERE name IN ('edited_at','deleted_at','reply_to')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("down 后消息新列应消失: %d %v", cols, err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master
		  WHERE (type='index' AND name='idx_messages_reply_to')
		     OR (type='table' AND name='message_mentions')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("down 后索引/表应消失: %d %v", tables, err)
	}

	// 再 up：列/索引/表回来，旧行仍是 NULL。
	if err := m.Migrate(ctx, until0013); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertLegacyMessage0013(t, m.DB())
}

// TestMessageModelMigrationMySQL 0013 up/down/up（DSN 不可用时跳过）。
func TestMessageModelMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	until0013 := migrationsBefore(t, full, "0014")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0013")); err != nil {
		t.Fatalf("迁移到 0012 失败: %v", err)
	}
	seedLegacy0013(t, m.DB())
	if err := m.Migrate(ctx, until0013); err != nil {
		t.Fatalf("应用 0013 失败: %v", err)
	}
	assertLegacyMessage0013(t, m.DB())
	insertProbe0013(t, m.DB())

	var cols int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = 'messages'
		    AND column_name IN ('edited_at','deleted_at','reply_to')`).Scan(&cols); err != nil || cols != 3 {
		t.Fatalf("up 后消息新列应存在: %d %v", cols, err)
	}
	for _, idx := range []struct{ table, name string }{
		{"messages", "idx_messages_reply_to"},
		{"message_mentions", "idx_message_mentions_user"},
	} {
		var n int
		if err := m.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.statistics
			  WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
			idx.table, idx.name).Scan(&n); err != nil || n == 0 {
			t.Fatalf("up 后索引 %s.%s 应存在: %d %v", idx.table, idx.name, n, err)
		}
	}
	var tables int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = 'message_mentions'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("up 后 message_mentions 应存在: %d %v", tables, err)
	}

	if err := m.RollbackLast(ctx, until0013); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	assertLegacyMessageContent0013(t, m.DB())
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = 'messages'
		    AND column_name IN ('edited_at','deleted_at','reply_to')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("down 后消息新列应消失: %d %v", cols, err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = 'message_mentions'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("down 后 message_mentions 应消失: %d %v", tables, err)
	}

	if err := m.Migrate(ctx, until0013); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertLegacyMessage0013(t, m.DB())
}
