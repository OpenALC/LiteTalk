package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// #38 B1：私聊/群私聊 = 无服务器频道（scope/direct/group + channel_members）。
// 本文件覆盖 0012 双方言迁移与 MySQL/SQLite/Mem 三实现契约。

// convSeq 保证同一二进制内用户 email/nickname 唯一（Mem 与真库用例共用）。
var convSeq atomic.Int64

// convUsers 建 n 个互相独立的用户（每次调用一组新数据）。
func convUsers(t *testing.T, st Store, n int) []*User {
	t.Helper()
	ctx := context.Background()
	seq := convSeq.Add(1)
	out := make([]*User, n)
	for i := range out {
		u := &User{
			Email:        fmt.Sprintf("conv-%d-%d@ex.test", seq, i),
			Nickname:     fmt.Sprintf("c%d-%d", seq%100000, i),
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

// wantMember 断言成员列表包含 userID 且角色/nickname 正确。
func wantMember(t *testing.T, list []ChannelMember, u *User, role ChannelMemberRole) {
	t.Helper()
	for _, m := range list {
		if m.UserID == u.ID {
			if m.Role != role {
				t.Fatalf("成员 %d 角色 = %s，期望 %s", u.ID, m.Role, role)
			}
			if m.Nickname != u.Nickname || m.Tag != u.Tag {
				t.Fatalf("成员 %d 内联信息 = %s#%s，期望 %s#%s",
					u.ID, m.Nickname, m.Tag, u.Nickname, u.Tag)
			}
			return
		}
	}
	t.Fatalf("成员列表缺少 %d: %+v", u.ID, list)
}

// runConversationsContract 三实现共用。
func runConversationsContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("direct 创建与幂等查找", func(t *testing.T) {
		us := convUsers(t, st, 3)
		a, b, outsider := us[0], us[1], us[2]

		conv := &Channel{Scope: ChannelScopeDirect, Name: "私聊"}
		if err := st.CreateConversation(ctx, conv, []int64{b.ID, a.ID}); err != nil {
			t.Fatalf("CreateConversation(direct): %v", err)
		}
		wantKey := fmt.Sprintf("%d:%d", min64(a.ID, b.ID), max64(a.ID, b.ID))
		if conv.ID == 0 || conv.CreatedAt.IsZero() {
			t.Fatalf("会话应有 id/created_at: %+v", conv)
		}
		if conv.ServerID != 0 || conv.Scope != ChannelScopeDirect || conv.Type != ChannelText ||
			conv.DMKey != wantKey || conv.CategoryID != nil || conv.ParentID != nil ||
			conv.PasswordHash != "" || conv.UserLimit != 0 || conv.Position != 0 {
			t.Fatalf("direct 规范化不符: %+v", conv)
		}

		for _, pair := range [][2]int64{{a.ID, b.ID}, {b.ID, a.ID}} {
			got, err := st.FindDirectChannel(ctx, pair[0], pair[1])
			if err != nil || got.ID != conv.ID || got.DMKey != wantKey || got.ServerID != 0 {
				t.Fatalf("FindDirectChannel(%d,%d) = %+v %v", pair[0], pair[1], got, err)
			}
		}
		if _, err := st.FindDirectChannel(ctx, a.ID, outsider.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的私聊应 ErrNotFound，实际 %v", err)
		}

		// 反向/正向重复创建都撞 dm_key 唯一冲突，且不得留下第二行。
		dup := &Channel{Scope: ChannelScopeDirect}
		if err := st.CreateConversation(ctx, dup, []int64{a.ID, b.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("重复私聊应 ErrConflict，实际 %v", err)
		}

		got, err := st.ChannelByID(ctx, conv.ID)
		if err != nil || got.Scope != ChannelScopeDirect || got.ServerID != 0 || got.DMKey != wantKey {
			t.Fatalf("ChannelByID(direct) = %+v %v", got, err)
		}

		members, err := st.ChannelMembers(ctx, conv.ID)
		if err != nil || len(members) != 2 {
			t.Fatalf("ChannelMembers = %+v %v", members, err)
		}
		wantMember(t, members, a, ChannelMemberMember)
		wantMember(t, members, b, ChannelMemberMember)

		cm, err := st.ChannelMember(ctx, conv.ID, a.ID)
		if err != nil || cm.Role != ChannelMemberMember || cm.Nickname != a.Nickname {
			t.Fatalf("ChannelMember = %+v %v", cm, err)
		}
		if _, err := st.ChannelMember(ctx, conv.ID, outsider.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("非成员 ChannelMember 应 ErrNotFound，实际 %v", err)
		}

		// 全行写保留会话身份：server_id 仍为 NULL（Go 0）、dm_key 不变。
		got.Name = "私聊2"
		if err := st.UpdateChannel(ctx, got); err != nil {
			t.Fatalf("UpdateChannel(direct): %v", err)
		}
		got2, err := st.ChannelByID(ctx, conv.ID)
		if err != nil || got2.Name != "私聊2" || got2.ServerID != 0 ||
			got2.Scope != ChannelScopeDirect || got2.DMKey != wantKey {
			t.Fatalf("会话全行写后身份丢失: %+v %v", got2, err)
		}

		list, err := st.ListConversations(ctx, a.ID)
		if err != nil || len(list) != 1 || list[0].ID != conv.ID {
			t.Fatalf("ListConversations(a) = %+v %v", list, err)
		}
		if list, err := st.ListConversations(ctx, outsider.ID); err != nil || len(list) != 0 {
			t.Fatalf("非成员会话列表应空: %+v %v", list, err)
		}
	})

	t.Run("group 成员管理与删除级联", func(t *testing.T) {
		us := convUsers(t, st, 4)
		owner, m1, m2, extra := us[0], us[1], us[2], us[3]

		conv := &Channel{Scope: ChannelScopeGroup, Name: "群"}
		if err := st.CreateConversation(ctx, conv, []int64{owner.ID, m1.ID, m2.ID}); err != nil {
			t.Fatalf("CreateConversation(group): %v", err)
		}
		if conv.ServerID != 0 || conv.Scope != ChannelScopeGroup || conv.DMKey != "" || conv.Type != ChannelText {
			t.Fatalf("group 规范化不符: %+v", conv)
		}
		if got, err := st.FindDirectChannel(ctx, owner.ID, m1.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("group 不应命中私聊查找: %+v %v", got, err)
		}

		members, err := st.ChannelMembers(ctx, conv.ID)
		if err != nil || len(members) != 3 {
			t.Fatalf("ChannelMembers = %+v %v", members, err)
		}
		wantMember(t, members, owner, ChannelMemberOwner)
		wantMember(t, members, m1, ChannelMemberMember)
		wantMember(t, members, m2, ChannelMemberMember)

		if err := st.AddChannelMember(ctx, conv.ID, extra.ID, ""); err != nil {
			t.Fatalf("AddChannelMember(默认角色): %v", err)
		}
		if cm, err := st.ChannelMember(ctx, conv.ID, extra.ID); err != nil || cm.Role != ChannelMemberMember {
			t.Fatalf("新增成员读回不符: %+v %v", cm, err)
		}
		if err := st.AddChannelMember(ctx, conv.ID, extra.ID, ChannelMemberMember); !errors.Is(err, ErrConflict) {
			t.Fatalf("重复成员应 ErrConflict，实际 %v", err)
		}
		if err := st.AddChannelMember(ctx, conv.ID, extra.ID, ChannelMemberRole("admin")); !errors.Is(err, ErrForbidden) {
			t.Fatalf("非法角色应 ErrForbidden，实际 %v", err)
		}

		if err := st.RemoveChannelMember(ctx, conv.ID, m1.ID); err != nil {
			t.Fatalf("RemoveChannelMember: %v", err)
		}
		if err := st.RemoveChannelMember(ctx, conv.ID, m1.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("重复移除应 ErrNotFound，实际 %v", err)
		}
		if _, err := st.ChannelMember(ctx, conv.ID, m1.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("被移除成员应 ErrNotFound，实际 %v", err)
		}
		if list, err := st.ListConversations(ctx, m1.ID); err != nil || len(list) != 0 {
			t.Fatalf("被移除成员会话列表应空: %+v %v", list, err)
		}
		if list, err := st.ListConversations(ctx, extra.ID); err != nil || len(list) != 1 {
			t.Fatalf("新成员会话列表应 1 条: %+v %v", list, err)
		}

		msg := &Message{ChannelID: conv.ID, UserID: owner.ID, Content: "hi"}
		if err := st.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
		if err := st.DeleteChannel(ctx, conv.ID); err != nil {
			t.Fatalf("DeleteChannel(group): %v", err)
		}
		if _, err := st.ChannelByID(ctx, conv.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("群会话应删除，实际 %v", err)
		}
		if msgs, err := st.MessagesBefore(ctx, conv.ID, 0, 10); err != nil || len(msgs) != 0 {
			t.Fatalf("群会话消息应级联删除: %+v %v", msgs, err)
		}
		if list, err := st.ChannelMembers(ctx, conv.ID); err != nil || len(list) != 0 {
			t.Fatalf("群会话成员应级联删除: %+v %v", list, err)
		}
	})

	t.Run("列表按创建时间倒序", func(t *testing.T) {
		us := convUsers(t, st, 3)
		a, b, c := us[0], us[1], us[2]
		first := &Channel{Scope: ChannelScopeDirect}
		if err := st.CreateConversation(ctx, first, []int64{a.ID, b.ID}); err != nil {
			t.Fatal(err)
		}
		second := &Channel{Scope: ChannelScopeGroup, Name: "后建"}
		if err := st.CreateConversation(ctx, second, []int64{a.ID, c.ID}); err != nil {
			t.Fatal(err)
		}
		list, err := st.ListConversations(ctx, a.ID)
		if err != nil || len(list) != 2 {
			t.Fatalf("ListConversations = %+v %v", list, err)
		}
		if list[0].ID != second.ID || list[1].ID != first.ID {
			t.Fatalf("应按 created_at/id 倒序: %+v", []int64{list[0].ID, list[1].ID})
		}
	})

	t.Run("server 频道回归与构造器边界", func(t *testing.T) {
		us := convUsers(t, st, 3)
		a, b := us[0], us[1]

		srv := &Server{Name: "conv-srv", OwnerID: a.ID, IconName: "music", IconColor: "#4FA4F3"}
		if err := st.CreateServer(ctx, srv); err != nil {
			t.Fatalf("CreateServer: %v", err)
		}
		ch := &Channel{ServerID: srv.ID, Name: "文字", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		if ch.Scope != ChannelScopeServer || ch.DMKey != "" {
			t.Fatalf("CreateChannel 未按 server 规范化: %+v", ch)
		}
		if got, err := st.ChannelByID(ctx, ch.ID); err != nil ||
			got.Scope != ChannelScopeServer || got.ServerID != srv.ID || got.DMKey != "" {
			t.Fatalf("server 频道读回不符: %+v %v", got, err)
		}

		// 会话不污染服务器频道列表。
		conv := &Channel{Scope: ChannelScopeGroup, Name: "g"}
		if err := st.CreateConversation(ctx, conv, []int64{a.ID, b.ID}); err != nil {
			t.Fatal(err)
		}
		list, err := st.ChannelsOfServer(ctx, srv.ID)
		if err != nil || len(list) != 1 || list[0].ID != ch.ID {
			t.Fatalf("ChannelsOfServer 应只含 server 频道: %+v %v", list, err)
		}

		// CreateChannel 拒绝会话作用域；CreateConversation 拒绝 server 与非 direct/group。
		if err := st.CreateChannel(ctx, &Channel{ServerID: srv.ID, Scope: ChannelScopeDirect}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("CreateChannel(direct) 应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateChannel(ctx, &Channel{Scope: ChannelScopeServer}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("server_id=0 的 CreateChannel 应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateConversation(ctx, &Channel{Scope: ChannelScopeServer}, []int64{a.ID, b.ID}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("CreateConversation(server) 应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateConversation(ctx, &Channel{Scope: ChannelScopeDirect}, []int64{a.ID, b.ID, us[2].ID}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("direct 三人应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateConversation(ctx, &Channel{Scope: ChannelScopeDirect}, []int64{a.ID, a.ID}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("direct 同一用户应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateConversation(ctx, &Channel{Scope: ChannelScopeGroup}, nil); !errors.Is(err, ErrForbidden) {
			t.Fatalf("group 零成员应 ErrForbidden，实际 %v", err)
		}

		// channel_members 只属于会话：server 频道拒绝。
		if err := st.AddChannelMember(ctx, ch.ID, a.ID, ChannelMemberMember); !errors.Is(err, ErrForbidden) {
			t.Fatalf("server 频道 AddChannelMember 应 ErrForbidden，实际 %v", err)
		}
		if err := st.AddChannelMember(ctx, 999999, a.ID, ChannelMemberMember); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在频道 AddChannelMember 应 ErrNotFound，实际 %v", err)
		}
	})
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func TestConversationsContractMem(t *testing.T) {
	runConversationsContract(t, NewMem())
}

func TestConversationsContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runConversationsContract(t, sq)
}

func TestConversationsContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runConversationsContract(t, m)
}

// ---- 0012 迁移（双方言） ----

// seedLegacy0012 预置 0011 及以前的形状：两用户/一服/一分类/父频道（带选项）/子频道/消息。
func seedLegacy0012(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES
		 (9101, 'conv9101@ex.test', 'conv9101', '0001', 'h'),
		 (9102, 'conv9102@ex.test', 'conv9102', '0002', 'h')`,
		`INSERT INTO servers (id, name, owner_id) VALUES (9201, 'legacy', 9101)`,
		`INSERT INTO categories (id, server_id, name, position) VALUES (9401, 9201, 'cat', 0)`,
		`INSERT INTO channels (id, server_id, name, type, category_id, position, password_hash, user_limit, parent_id)
		 VALUES (9301, 9201, 'parent', 'voice', 9401, 1, 'hash-9301', 5, NULL),
		        (9302, 9201, 'child',  'text',  9401, 2, '',         0, 9301)`,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (9501, 9301, 9101, 'legacy')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("预置失败: %v", err)
		}
	}
}

// assertLegacyRows0012 旧频道/消息必须原样保留（down 后 scope/dm_key 已不存在）。
func assertLegacyRows0012(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var serverID int64
	var hash string
	var limit int
	var catID, parentID sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT server_id, password_hash, user_limit, category_id, parent_id
		   FROM channels WHERE id = 9301`).
		Scan(&serverID, &hash, &limit, &catID, &parentID); err != nil {
		t.Fatalf("旧频道读回失败: %v", err)
	}
	if serverID != 9201 || hash != "hash-9301" || limit != 5 ||
		!catID.Valid || catID.Int64 != 9401 || parentID.Valid {
		t.Fatalf("旧频道数据漂移: server=%d hash=%s limit=%d cat=%v parent=%v",
			serverID, hash, limit, catID, parentID)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT parent_id FROM channels WHERE id = 9302`).Scan(&parentID); err != nil ||
		!parentID.Valid || parentID.Int64 != 9301 {
		t.Fatalf("子频道父级丢失: %v %v", parentID, err)
	}
	var content string
	if err := db.QueryRowContext(ctx,
		`SELECT content FROM messages WHERE id = 9501`).Scan(&content); err != nil || content != "legacy" {
		t.Fatalf("旧消息应保留: %q %v", content, err)
	}
}

// assertLegacy0012 旧行在 up 后还必须是 server 作用域、dm_key NULL。
func assertLegacy0012(t *testing.T, db *sql.DB) {
	t.Helper()
	assertLegacyRows0012(t, db)
	ctx := context.Background()
	var scope string
	var dmKey sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT scope, dm_key FROM channels WHERE id = 9301`).Scan(&scope, &dmKey); err != nil {
		t.Fatalf("旧频道作用域读回失败: %v", err)
	}
	if scope != "server" || dmKey.Valid {
		t.Fatalf("旧频道应补成 server/NULL: scope=%s dm=%v", scope, dmKey)
	}
}

// insertConversationRows0012 直接写两行会话（绕过 Store，验证迁移形状）：
// 6001 direct（dm_key 唯一）、6002 group（dm_key NULL）、6003 另一个 group（NULL 可重复）。
func insertConversationRows0012(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO channels (id, server_id, name, type, position, scope, dm_key)
		 VALUES (6001, NULL, '', 'text', 0, 'direct', '9101:9102')`,
		`INSERT INTO channels (id, server_id, name, type, position, scope, dm_key)
		 VALUES (6002, NULL, '群', 'text', 0, 'group', NULL)`,
		`INSERT INTO channels (id, server_id, name, type, position, scope, dm_key)
		 VALUES (6003, NULL, '群2', 'text', 0, 'group', NULL)`,
		`INSERT INTO channel_members (channel_id, user_id, role, joined_at)
		 VALUES (6001, 9101, 'member', CURRENT_TIMESTAMP),
		        (6001, 9102, 'member', CURRENT_TIMESTAMP),
		        (6002, 9101, 'owner', CURRENT_TIMESTAMP)`,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (9601, 6001, 9101, 'dm')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("写会话行失败: %v", err)
		}
	}
	// dm_key 唯一：重复 direct 行必须被拒。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, position, scope, dm_key)
		 VALUES (6004, NULL, '', 'text', 0, 'direct', '9101:9102')`); err == nil {
		t.Fatal("重复 dm_key 应被唯一索引拒绝")
	}
	var members, messages int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channel_members WHERE channel_id = 6001`).Scan(&members); err != nil || members != 2 {
		t.Fatalf("channel_members 读写不符: %d %v", members, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE channel_id = 6001`).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("会话消息应可写入: %d %v", messages, err)
	}
}

// assertConversationsGone0012 down 后：非 server 频道与其消息消失，channel_members 消失。
func assertConversationsGone0012(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var channels, messages int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channels WHERE id IN (6001, 6002, 6003)`).Scan(&channels); err != nil || channels != 0 {
		t.Fatalf("down 后会话频道应消失: %d %v", channels, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE id = 9601`).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("down 后会话消息应消失: %d %v", messages, err)
	}
	assertLegacyRows0012(t, db)
	// server_id 恢复 NOT NULL。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type) VALUES (6005, NULL, 'x', 'text')`); err == nil {
		t.Fatal("down 后 server_id 应恢复 NOT NULL")
	}
}

// TestConversationsMigrationSQLite 0012 up/down/up（SQLite 无条件跑）。
func TestConversationsMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	until0012 := migrationsBefore(t, full, "0013")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0012")); err != nil {
		t.Fatalf("迁移到 0011 失败: %v", err)
	}
	seedLegacy0012(t, m.DB())
	if err := m.Migrate(ctx, until0012); err != nil {
		t.Fatalf("应用 0012 失败: %v", err)
	}
	assertLegacy0012(t, m.DB())
	insertConversationRows0012(t, m.DB())
	// server_id 可空 + 新列存在。
	var notNull int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT "notnull" FROM pragma_table_info('channels') WHERE name = 'server_id'`).Scan(&notNull); err != nil || notNull != 0 {
		t.Fatalf("up 后 server_id 应可空: %d %v", notNull, err)
	}
	var cols int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name IN ('scope','dm_key')`).Scan(&cols); err != nil || cols != 2 {
		t.Fatalf("up 后 scope/dm_key 应存在: %d %v", cols, err)
	}

	if err := m.RollbackLast(ctx, until0012); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	assertConversationsGone0012(t, m.DB())
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name IN ('scope','dm_key')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("down 后 scope/dm_key 应消失: %d %v", cols, err)
	}
	var tables int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='channel_members'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("down 后 channel_members 应消失: %d %v", tables, err)
	}

	// 再 up：列/表回来，旧行仍是 server 作用域。
	if err := m.Migrate(ctx, until0012); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertLegacy0012(t, m.DB())
	insertConversationRows0012(t, m.DB())
}

// TestConversationsMigrationMySQL 0012 up/down/up（DSN 不可用时跳过）。
func TestConversationsMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	until0012 := migrationsBefore(t, full, "0013")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0012")); err != nil {
		t.Fatalf("迁移到 0011 失败: %v", err)
	}
	seedLegacy0012(t, m.DB())
	if err := m.Migrate(ctx, until0012); err != nil {
		t.Fatalf("应用 0012 失败: %v", err)
	}
	assertLegacy0012(t, m.DB())
	insertConversationRows0012(t, m.DB())
	var nullable string
	if err := m.DB().QueryRowContext(ctx,
		`SELECT IS_NULLABLE FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = 'channels' AND column_name = 'server_id'`).
		Scan(&nullable); err != nil || nullable != "YES" {
		t.Fatalf("up 后 server_id 应可空: %q %v", nullable, err)
	}
	var cols int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = 'channels'
		    AND column_name IN ('scope','dm_key')`).Scan(&cols); err != nil || cols != 2 {
		t.Fatalf("up 后 scope/dm_key 应存在: %d %v", cols, err)
	}

	if err := m.RollbackLast(ctx, until0012); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	assertConversationsGone0012(t, m.DB())
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = 'channels'
		    AND column_name IN ('scope','dm_key')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("down 后 scope/dm_key 应消失: %d %v", cols, err)
	}
	var tables int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = 'channel_members'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("down 后 channel_members 应消失: %d %v", tables, err)
	}

	if err := m.Migrate(ctx, until0012); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertLegacy0012(t, m.DB())
	insertConversationRows0012(t, m.DB())
}
