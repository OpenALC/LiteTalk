package store

import (
	"context"
	"errors"
	"testing"
)

// runMessagesNoncePaging 消息游标分页 + nonce 幂等语义（#31），Mem 与 SQLite 共用。
func runMessagesNoncePaging(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	u := &User{Email: "m@ex.com", Nickname: "m", Tag: "0001"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sv := &Server{Name: "s", OwnerID: u.ID, IconName: "hash", IconColor: "#fff"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	ch := &Channel{ServerID: sv.ID, Name: "c", Type: ChannelText}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	ch2 := &Channel{ServerID: sv.ID, Name: "c2", Type: ChannelText}
	if err := st.CreateChannel(ctx, ch2); err != nil {
		t.Fatal(err)
	}

	// nonce 幂等：同频道同用户同 nonce 撞唯一约束；空 nonce 不去重。
	a := &Message{ChannelID: ch.ID, UserID: u.ID, Content: "一", Nonce: "n1"}
	if err := st.CreateMessage(ctx, a); err != nil {
		t.Fatal(err)
	}
	dup := &Message{ChannelID: ch.ID, UserID: u.ID, Content: "二", Nonce: "n1"}
	if err := st.CreateMessage(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("同 nonce 应 ErrConflict，实际 %v", err)
	}
	got, err := st.MessageByNonce(ctx, ch.ID, u.ID, "n1")
	if err != nil || got.Content != "一" || got.ID != a.ID {
		t.Fatalf("幂等回读应为原消息：%+v %v", got, err)
	}
	if _, err := st.MessageByNonce(ctx, ch.ID, u.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中应 ErrNotFound，实际 %v", err)
	}
	// 去重键含频道：同用户同 nonce 在另一个频道独立成条，回读不串频道。
	b := &Message{ChannelID: ch2.ID, UserID: u.ID, Content: "另频道", Nonce: "n1"}
	if err := st.CreateMessage(ctx, b); err != nil {
		t.Fatalf("跨频道同 nonce 应独立落库: %v", err)
	}
	if orig, err := st.MessageByNonce(ctx, ch.ID, u.ID, "n1"); err != nil || orig.ID != a.ID {
		t.Fatalf("原频道回读串到了别的频道：%+v %v", orig, err)
	}
	if other, err := st.MessageByNonce(ctx, ch2.ID, u.ID, "n1"); err != nil || other.ID != b.ID || other.ChannelID != ch2.ID {
		t.Fatalf("跨频道回读应为第二条：%+v %v", other, err)
	}
	if _, err := st.MessageByNonce(ctx, ch2.ID, u.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨频道未命中应 ErrNotFound，实际 %v", err)
	}
	for _, c := range []string{"", ""} {
		if err := st.CreateMessage(ctx, &Message{ChannelID: ch.ID, UserID: u.ID, Content: "空" + c}); err != nil {
			t.Fatalf("空 nonce 不应去重: %v", err)
		}
	}

	// 追加 3 条带独立 nonce 的消息（落库共 6 条：a + 2 空 nonce + 3 条）。
	for i := 0; i < 3; i++ {
		m := &Message{ChannelID: ch.ID, UserID: u.ID, Content: "x", Nonce: string(rune('a' + i))}
		if err := st.CreateMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.MessagesBefore(ctx, ch.ID, 0, 50)
	if err != nil || len(all) != 6 {
		t.Fatalf("全量应 6 条：%d %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("MessagesBefore 应正序返回：%v", all)
		}
	}

	// before=第4条 → 最旧 3 条；after=第2条 → 第3条起 4 条。
	before, err := st.MessagesBefore(ctx, ch.ID, all[3].ID, 50)
	if err != nil || len(before) != 3 || before[0].ID != all[0].ID {
		t.Fatalf("before 分页：%d %v", len(before), err)
	}
	after, err := st.MessagesAfter(ctx, ch.ID, all[1].ID, 50)
	if err != nil || len(after) != 4 || after[0].ID != all[2].ID {
		t.Fatalf("after 分页：%d %v", len(after), err)
	}
	// limit 截断：after 页取 2 条。
	two, err := st.MessagesAfter(ctx, ch.ID, all[1].ID, 2)
	if err != nil || len(two) != 2 || two[0].ID != all[2].ID {
		t.Fatalf("after limit：%d %v", len(two), err)
	}
}

// TestMemMessagesNoncePaging Mem 实现语义。
func TestMemMessagesNoncePaging(t *testing.T) {
	runMessagesNoncePaging(t, NewMem())
}

// TestSQLiteMessagesNoncePaging SQLite 实现语义（含唯一索引与 COALESCE 扫描）。
func TestSQLiteMessagesNoncePaging(t *testing.T) {
	m := openTestSQLite(t)
	if err := m.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runMessagesNoncePaging(t, m)
}
