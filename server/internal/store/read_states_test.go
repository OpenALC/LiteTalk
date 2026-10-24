package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// #40 B1：未读与提及计数（迁移 0014 read_states）。本文件覆盖双方言迁移与
// MySQL/SQLite/Mem 三实现契约。

// readStateSeq 保证同一二进制内用户 email/nickname 唯一（Mem 与真库用例共用）。
var readStateSeq atomic.Int64

// readStateUsers 建 n 个互相独立的用户（每次调用一组新数据）。
func readStateUsers(t *testing.T, st Store, n int) []*User {
	t.Helper()
	ctx := context.Background()
	seq := readStateSeq.Add(1)
	out := make([]*User, n)
	for i := range out {
		u := &User{
			Email:        fmt.Sprintf("read-%d-%d@ex.test", seq, i),
			Nickname:     fmt.Sprintf("rs%d-%d", seq%100000, i),
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

// readStateChannel 在服务器下建一个文字频道（每个子用例独立频道，互不干扰）。
func readStateChannel(t *testing.T, st Store, serverID int64, name string) *Channel {
	t.Helper()
	ch := &Channel{ServerID: serverID, Name: name, Type: ChannelText}
	if err := st.CreateChannel(context.Background(), ch); err != nil {
		t.Fatalf("CreateChannel(%s): %v", name, err)
	}
	return ch
}

// runReadStatesContract 三实现共用契约。
func runReadStatesContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	us := readStateUsers(t, st, 3)
	owner, other, third := us[0], us[1], us[2]
	sv := &Server{Name: "read-state", OwnerID: owner.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	t.Run("水位读写与只前进", func(t *testing.T) {
		ch := readStateChannel(t, st, sv.ID, "watermark")

		if _, err := st.ReadState(ctx, owner.ID, ch.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("无记录应 ErrNotFound，实际 %v", err)
		}
		// Ensure 缺失时插入。
		if err := st.EnsureReadState(ctx, owner.ID, ch.ID, 10); err != nil {
			t.Fatalf("EnsureReadState(插入): %v", err)
		}
		if got, err := st.ReadState(ctx, owner.ID, ch.ID); err != nil || got != 10 {
			t.Fatalf("Ensure 插入后水位 = %d %v，期望 10", got, err)
		}
		// Ensure 不覆盖已有值（更低和更高都不动）。
		for _, v := range []int64{5, 99} {
			if err := st.EnsureReadState(ctx, owner.ID, ch.ID, v); err != nil {
				t.Fatalf("EnsureReadState(%d): %v", v, err)
			}
			if got, err := st.ReadState(ctx, owner.ID, ch.ID); err != nil || got != 10 {
				t.Fatalf("Ensure 不应覆盖已有水位：%d %v，期望 10", got, err)
			}
		}
		// Set 前进。
		if err := st.SetReadState(ctx, owner.ID, ch.ID, 20); err != nil {
			t.Fatalf("SetReadState(前进): %v", err)
		}
		if got, err := st.ReadState(ctx, owner.ID, ch.ID); err != nil || got != 20 {
			t.Fatalf("Set 前进后水位 = %d %v，期望 20", got, err)
		}
		// Set 不回退（乱序/重复上报）。
		for _, v := range []int64{15, 20} {
			if err := st.SetReadState(ctx, owner.ID, ch.ID, v); err != nil {
				t.Fatalf("SetReadState(%d): %v", v, err)
			}
			if got, err := st.ReadState(ctx, owner.ID, ch.ID); err != nil || got != 20 {
				t.Fatalf("Set 不应回退水位：%d %v，期望 20", got, err)
			}
		}
		// 用户维度隔离。
		if _, err := st.ReadState(ctx, other.ID, ch.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("其他用户不应有水位，实际 %v", err)
		}
		// 首次 Set 即可建行。
		if err := st.SetReadState(ctx, other.ID, ch.ID, 7); err != nil {
			t.Fatalf("SetReadState(首次): %v", err)
		}
		if got, err := st.ReadState(ctx, other.ID, ch.ID); err != nil || got != 7 {
			t.Fatalf("首次 Set 后水位 = %d %v，期望 7", got, err)
		}
	})

	t.Run("未读排除自己消息与软删消息", func(t *testing.T) {
		ch := readStateChannel(t, st, sv.ID, "unread")
		// 顺序：本人 → 他人（提及本人两次，Store 去重）→ 本人 → 他人（提及本人，软删）。
		m1 := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "自己"}
		if err := st.CreateMessage(ctx, m1); err != nil {
			t.Fatalf("CreateMessage(m1): %v", err)
		}
		m2 := &Message{
			ChannelID: ch.ID, UserID: other.ID, Content: "别人",
			Mentions: []int64{owner.ID, owner.ID},
		}
		if err := st.CreateMessage(ctx, m2); err != nil {
			t.Fatalf("CreateMessage(m2): %v", err)
		}
		m3 := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "自己2"}
		if err := st.CreateMessage(ctx, m3); err != nil {
			t.Fatalf("CreateMessage(m3): %v", err)
		}
		m4 := &Message{
			ChannelID: ch.ID, UserID: other.ID, Content: "待删",
			Mentions: []int64{owner.ID},
		}
		if err := st.CreateMessage(ctx, m4); err != nil {
			t.Fatalf("CreateMessage(m4): %v", err)
		}
		if err := st.SoftDeleteMessage(ctx, m4.ID, time.Now()); err != nil {
			t.Fatalf("SoftDeleteMessage: %v", err)
		}

		// owner 无水位行：水位 0，只有 m2 算未读且提及 1（m1/m3 本人，m4 软删）。
		got, err := st.UnreadCounts(ctx, owner.ID, []int64{ch.ID})
		if err != nil {
			t.Fatalf("UnreadCounts(owner): %v", err)
		}
		if got[ch.ID] != (Unread{UnreadCount: 1, MentionCount: 1}) {
			t.Fatalf("owner 未读 = %+v，期望 1/1", got[ch.ID])
		}
		// other 无水位行：m1、m3 两条（自己的 m2/m4 不算；m4 已删）。
		got, err = st.UnreadCounts(ctx, other.ID, []int64{ch.ID})
		if err != nil {
			t.Fatalf("UnreadCounts(other): %v", err)
		}
		if got[ch.ID] != (Unread{UnreadCount: 2, MentionCount: 0}) {
			t.Fatalf("other 未读 = %+v，期望 2/0", got[ch.ID])
		}
		// owner 读到 m2：未读清零（m3 本人、m4 已删）。
		if err := st.SetReadState(ctx, owner.ID, ch.ID, m2.ID); err != nil {
			t.Fatalf("SetReadState(m2): %v", err)
		}
		got, err = st.UnreadCounts(ctx, owner.ID, []int64{ch.ID})
		if err != nil {
			t.Fatalf("UnreadCounts(owner, 读到 m2): %v", err)
		}
		if got[ch.ID] != (Unread{}) {
			t.Fatalf("读到 m2 后 owner 未读 = %+v，期望 0/0", got[ch.ID])
		}
	})

	t.Run("多频道批量统计", func(t *testing.T) {
		chA := readStateChannel(t, st, sv.ID, "multi-a")
		chB := readStateChannel(t, st, sv.ID, "multi-b")
		for i := 0; i < 2; i++ {
			m := &Message{ChannelID: chA.ID, UserID: other.ID, Content: "a"}
			if err := st.CreateMessage(ctx, m); err != nil {
				t.Fatalf("CreateMessage(a%d): %v", i, err)
			}
		}
		read := &Message{ChannelID: chB.ID, UserID: other.ID, Content: "b"}
		if err := st.CreateMessage(ctx, read); err != nil {
			t.Fatalf("CreateMessage(b): %v", err)
		}
		if err := st.SetReadState(ctx, owner.ID, chB.ID, read.ID); err != nil {
			t.Fatalf("SetReadState(chB): %v", err)
		}

		got, err := st.UnreadCounts(ctx, owner.ID, []int64{chA.ID, chB.ID, 999999999})
		if err != nil {
			t.Fatalf("UnreadCounts: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("请求项应全部返回（含 0 值频道）: %+v", got)
		}
		if got[chA.ID] != (Unread{UnreadCount: 2}) || got[chB.ID] != (Unread{}) || got[999999999] != (Unread{}) {
			t.Fatalf("多频道未读不符: %+v", got)
		}
		if empty, err := st.UnreadCounts(ctx, owner.ID, nil); err != nil || len(empty) != 0 {
			t.Fatalf("空入参应返回空 map: %+v %v", empty, err)
		}
	})

	t.Run("成员维度批量统计", func(t *testing.T) {
		ch := readStateChannel(t, st, sv.ID, "members")
		// owner 发两条：一条提及 other、一条提及 third；other 无水位行、third 用 Ensure 建行。
		m1 := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "at-other",
			Mentions: []int64{other.ID},
		}
		if err := st.CreateMessage(ctx, m1); err != nil {
			t.Fatalf("CreateMessage(m1): %v", err)
		}
		m2 := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "at-third",
			Mentions: []int64{third.ID},
		}
		if err := st.CreateMessage(ctx, m2); err != nil {
			t.Fatalf("CreateMessage(m2): %v", err)
		}
		if err := st.EnsureReadState(ctx, third.ID, ch.ID, m1.ID); err != nil {
			t.Fatalf("EnsureReadState(third): %v", err)
		}

		got, err := st.UnreadCountsForUsers(ctx, ch.ID, []int64{owner.ID, other.ID, third.ID})
		if err != nil {
			t.Fatalf("UnreadCountsForUsers: %v", err)
		}
		// owner 只看别人的消息 → 0；other 无 read_states 行 → 水位 0，两条未读、提及 1；
		// third 水位 = m1 → 只剩 m2 未读且 m2 提及 third → 1/1。
		if len(got) != 3 {
			t.Fatalf("用户应全部返回: %+v", got)
		}
		if got[owner.ID] != (Unread{}) {
			t.Fatalf("owner 未读 = %+v，期望 0/0", got[owner.ID])
		}
		if got[other.ID] != (Unread{UnreadCount: 2, MentionCount: 1}) {
			t.Fatalf("other 未读 = %+v，期望 2/1", got[other.ID])
		}
		if got[third.ID] != (Unread{UnreadCount: 1, MentionCount: 1}) {
			t.Fatalf("third 未读 = %+v，期望 1/1", got[third.ID])
		}
		if empty, err := st.UnreadCountsForUsers(ctx, ch.ID, nil); err != nil || len(empty) != 0 {
			t.Fatalf("空入参应返回空 map: %+v %v", empty, err)
		}
	})

	// 频道无消息时请求项也应保留 0 值（三种实现同口径，供前端直接展示）。
	t.Run("空频道返回零值", func(t *testing.T) {
		ch := readStateChannel(t, st, sv.ID, "empty")
		got, err := st.UnreadCounts(ctx, owner.ID, []int64{ch.ID})
		if err != nil || got[ch.ID] != (Unread{}) {
			t.Fatalf("空频道未读 = %+v %v，期望 0/0", got, err)
		}
	})
}

func TestReadStatesContractMem(t *testing.T) {
	runReadStatesContract(t, NewMem())
}

func TestReadStatesContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runReadStatesContract(t, sq)
}

func TestReadStatesContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runReadStatesContract(t, m)
}

// ---- 0014 迁移（双方言） ----

// seedLegacy0014 预置 0013 及以前的形状：
// 两用户一服（owner/member）、一个 server 文字频道（两条消息）、一个 voice 频道
// （一条消息，验证回填只覆盖文本频道）、一个 direct 会话（成员两人、一条消息）。
func seedLegacy0014(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES
		 (4101, 'read4101@ex.test', 'read4101', '0001', 'h'),
		 (4102, 'read4102@ex.test', 'read4102', '0002', 'h')`,
		`INSERT INTO servers (id, name, owner_id) VALUES (4201, 'read-legacy', 4101)`,
		`INSERT INTO server_members (server_id, user_id, role) VALUES
		 (4201, 4101, 'owner'), (4201, 4102, 'member')`,
		`INSERT INTO channels (id, server_id, name, type, position, scope) VALUES
		 (4301, 4201, 'text', 'text', 0, 'server'),
		 (4302, 4201, 'voice', 'voice', 1, 'server')`,
		`INSERT INTO channels (id, server_id, name, type, position, scope, dm_key) VALUES
		 (4401, NULL, '', 'text', 0, 'direct', '4101:4102')`,
		`INSERT INTO channel_members (channel_id, user_id, role) VALUES
		 (4401, 4101, 'member'), (4401, 4102, 'member')`,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES
		 (4501, 4301, 4101, 'a'),
		 (4502, 4301, 4102, 'b'),
		 (4503, 4302, 4102, 'voice'),
		 (4601, 4401, 4101, 'dm')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("预置失败: %v", err)
		}
	}
}

// readStateWatermark 读回水位；无行返回 -1。
func readStateWatermark(t *testing.T, db *sql.DB, userID, channelID int64) int64 {
	t.Helper()
	var last int64
	err := db.QueryRowContext(context.Background(),
		`SELECT last_read_message_id FROM read_states WHERE user_id = ? AND channel_id = ?`,
		userID, channelID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return -1
	}
	if err != nil {
		t.Fatalf("读 read_states(%d,%d) 失败: %v", userID, channelID, err)
	}
	return last
}

// assertReadStatesBackfilled0014 up 后：服务器文本频道与 direct 各展开到全部成员，
// 水位 = 频道当前 MAX(messages.id)；voice 频道不回填。
func assertReadStatesBackfilled0014(t *testing.T, db *sql.DB) {
	t.Helper()
	cases := []struct {
		userID, channelID, want int64
	}{
		{4101, 4301, 4502}, // server 文本频道：两成员同水位
		{4102, 4301, 4502},
		{4101, 4401, 4601}, // direct：两成员同水位
		{4102, 4401, 4601},
		{4101, 4302, -1}, // voice 频道不回填
		{4102, 4302, -1},
	}
	for _, tc := range cases {
		if got := readStateWatermark(t, db, tc.userID, tc.channelID); got != tc.want {
			t.Fatalf("回填水位(%d,%d) = %d，期望 %d", tc.userID, tc.channelID, got, tc.want)
		}
	}
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM read_states`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("回填行数 = %d %v，期望 4", n, err)
	}
}

// TestReadStatesMigrationSQLite 0014 up/down/up（SQLite 无条件跑）。
func TestReadStatesMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	until0014 := migrationsBefore(t, full, "0015")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0014")); err != nil {
		t.Fatalf("迁移到 0013 失败: %v", err)
	}
	seedLegacy0014(t, m.DB())
	if err := m.Migrate(ctx, until0014); err != nil {
		t.Fatalf("应用 0014 失败: %v", err)
	}
	assertReadStatesBackfilled0014(t, m.DB())

	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_read_states_channel'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("up 后 idx_read_states_channel 应存在: %d %v", n, err)
	}
	// 主键去重。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO read_states (user_id, channel_id, last_read_message_id) VALUES (4101, 4301, 9999)`); err == nil {
		t.Fatal("重复 (user_id, channel_id) 应被主键拒绝")
	}

	// 回填后新增一条他人消息：对应成员未读 +1，发送者自己仍为 0。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (4700, 4301, 4102, 'new')`); err != nil {
		t.Fatalf("写新消息失败: %v", err)
	}
	got, err := m.UnreadCounts(ctx, 4101, []int64{4301})
	if err != nil {
		t.Fatalf("UnreadCounts: %v", err)
	}
	if got[4301] != (Unread{UnreadCount: 1}) {
		t.Fatalf("回填后新增消息的未读不符: %+v", got)
	}
	if got, err := m.UnreadCounts(ctx, 4102, []int64{4301}); err != nil || got[4301] != (Unread{}) {
		t.Fatalf("发送者自己的消息不应算未读: %+v %v", got, err)
	}

	if err := m.RollbackLast(ctx, until0014); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='read_states'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down 后 read_states 应消失: %d %v", n, err)
	}

	// 再 up：表回来并重新回填（此时 4301 的 MAX(id) 已是 4700）。
	if err := m.Migrate(ctx, until0014); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	if got := readStateWatermark(t, m.DB(), 4101, 4301); got != 4700 {
		t.Fatalf("再 up 回填水位 = %d，期望 4700", got)
	}
}

// TestReadStatesMigrationMySQL 0014 up/down/up（DSN 不可用时跳过）。
func TestReadStatesMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	until0014 := migrationsBefore(t, full, "0015")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0014")); err != nil {
		t.Fatalf("迁移到 0013 失败: %v", err)
	}
	seedLegacy0014(t, m.DB())
	if err := m.Migrate(ctx, until0014); err != nil {
		t.Fatalf("应用 0014 失败: %v", err)
	}
	assertReadStatesBackfilled0014(t, m.DB())

	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.statistics
		  WHERE table_schema = DATABASE() AND table_name = 'read_states'
		    AND index_name = 'idx_read_states_channel'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("up 后 idx_read_states_channel 应存在: %d %v", n, err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO read_states (user_id, channel_id, last_read_message_id) VALUES (4101, 4301, 9999)`); err == nil {
		t.Fatal("重复 (user_id, channel_id) 应被主键拒绝")
	}

	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content) VALUES (4700, 4301, 4102, 'new')`); err != nil {
		t.Fatalf("写新消息失败: %v", err)
	}
	got, err := m.UnreadCounts(ctx, 4101, []int64{4301})
	if err != nil {
		t.Fatalf("UnreadCounts: %v", err)
	}
	if got[4301] != (Unread{UnreadCount: 1}) {
		t.Fatalf("回填后新增消息的未读不符: %+v", got)
	}

	if err := m.RollbackLast(ctx, until0014); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = 'read_states'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down 后 read_states 应消失: %d %v", n, err)
	}

	if err := m.Migrate(ctx, until0014); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	if got := readStateWatermark(t, m.DB(), 4101, 4301); got != 4700 {
		t.Fatalf("再 up 回填水位 = %d，期望 4700", got)
	}
}
