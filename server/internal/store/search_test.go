package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// #42 B1：SearchMessages 三实现共享契约。

// searchFixture 建两个用户、一个服务器与三个频道；只把 ch1/ch2 当作可见集合。
type searchFixture struct {
	channelIDs []int64
	ch1, ch2   int64
	userID     int64
	msgIDs     map[string]int64
}

func newSearchFixture(t *testing.T, st Store) *searchFixture {
	t.Helper()
	ctx := context.Background()
	u1 := &User{Email: "search-a@ex.com", Nickname: "搜索甲", Tag: "0001"}
	if err := st.CreateUser(ctx, u1); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u2 := &User{Email: "search-b@ex.com", Nickname: "搜索乙", Tag: "0002"}
	if err := st.CreateUser(ctx, u2); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sv := &Server{Name: "搜索服", OwnerID: u1.ID, IconName: "hash", IconColor: "#fff"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	ch1 := &Channel{ServerID: sv.ID, Name: "频道一", Type: ChannelText}
	ch2 := &Channel{ServerID: sv.ID, Name: "频道二", Type: ChannelText}
	ch3 := &Channel{ServerID: sv.ID, Name: "频道三", Type: ChannelText}
	for _, ch := range []*Channel{ch1, ch2, ch3} {
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
	}
	f := &searchFixture{channelIDs: []int64{ch1.ID, ch2.ID}, ch1: ch1.ID, ch2: ch2.ID,
		userID: u1.ID, msgIDs: map[string]int64{}}
	seed := []struct {
		key     string
		channel int64
		user    int64
		content string
	}{
		{"zh3", ch1.ID, u1.ID, "你好世界 hello"},
		{"zh2", ch1.ID, u2.ID, "世界和平 hello again"},
		{"ch2", ch2.ID, u1.ID, "中文搜索样例"},
		{"hidden", ch3.ID, u1.ID, "中文搜索样例·不可见频道"},
		{"tomb", ch1.ID, u1.ID, "待删除的墓碑词"},
	}
	for _, s := range seed {
		msg := &Message{ChannelID: s.channel, UserID: s.user, Content: s.content}
		if err := st.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage(%s): %v", s.key, err)
		}
		f.msgIDs[s.key] = msg.ID
	}
	if err := st.SoftDeleteMessage(ctx, f.msgIDs["tomb"], time.Now()); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	return f
}

func searchIDs(t *testing.T, st Store, channelIDs []int64, query string, limit int) []int64 {
	t.Helper()
	msgs, err := st.SearchMessages(context.Background(), channelIDs, query, limit)
	if err != nil {
		t.Fatalf("SearchMessages(%q): %v", query, err)
	}
	ids := make([]int64, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	return ids
}

func assertSearchIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("命中 %v，期望 %v", got, want)
	}
	seen := make(map[int64]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] == 0 {
			t.Fatalf("命中 %v，缺少 %d（期望 %v）", got, id, want)
		}
	}
}

func runSearchMessages(t *testing.T, st Store) {
	ctx := context.Background()
	f := newSearchFixture(t, st)

	// 空频道集合 / 空查询：空结果且不报错。
	for _, tc := range []struct {
		name     string
		channels []int64
		query    string
	}{
		{"空频道", nil, "hello"},
		{"空查询", f.channelIDs, ""},
		{"纯空白查询", f.channelIDs, "   \t "},
	} {
		msgs, err := st.SearchMessages(ctx, tc.channels, tc.query, 20)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(msgs) != 0 {
			t.Fatalf("%s 应返回空结果，实际 %+v", tc.name, msgs)
		}
	}

	// 英文大小写不敏感；只命中传入频道。
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "HELLO", 20), f.msgIDs["zh3"], f.msgIDs["zh2"])
	assertSearchIDs(t, searchIDs(t, st, []int64{f.ch2}, "hello", 20))
	// 3 字符中文走 FTS/子串；2 字符与 1 字符走短查询回退。
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "你好世", 20), f.msgIDs["zh3"])
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "世界", 20), f.msgIDs["zh3"], f.msgIDs["zh2"])
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "好", 20), f.msgIDs["zh3"])
	// 跨频道：同一关键词只出现在 ch2 时，传 ch1 不命中。
	assertSearchIDs(t, searchIDs(t, st, []int64{f.ch1}, "中文搜索", 20))
	// 不可见频道（未传入集合）不得出现。
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "中文搜索", 20), f.msgIDs["ch2"])
	// 软删消息正文已清空，旧词不可搜。
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "墓碑词", 20))

	// 编辑后索引同步：旧词消失、新词可搜。
	if err := st.UpdateMessageContent(ctx, f.msgIDs["ch2"], "更新后的独特关键词", nil, time.Now()); err != nil {
		t.Fatalf("UpdateMessageContent: %v", err)
	}
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "中文搜索", 20))
	assertSearchIDs(t, searchIDs(t, st, f.channelIDs, "独特关键词", 20), f.msgIDs["ch2"])

	// 发送者内联信息与结果去重。
	msgs, err := st.SearchMessages(ctx, f.channelIDs, "hello", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if msg.SenderNickname == "" || msg.SenderTag == "" {
			t.Fatalf("结果缺少发送者内联信息: %+v", msg)
		}
	}
	seen := map[int64]struct{}{}
	for _, msg := range msgs {
		if _, dup := seen[msg.ID]; dup {
			t.Fatalf("结果重复: %v", msgs)
		}
		seen[msg.ID] = struct{}{}
	}

	// 特殊字符不能变成 FTS 语法错误（短语包裹 + LIKE 转义）：只要不报错即可。
	for _, q := range []string{`"`, `"""`, `a"b"c`, `***`, `AND OR NOT`, `NEAR(foo bar)`, `(paren)`, `-minus`, `^caret`, `back\slash`, `100%_under`} {
		if _, err := st.SearchMessages(ctx, f.channelIDs, q, 20); err != nil {
			t.Fatalf("特殊查询 %q 不应报错: %v", q, err)
		}
	}

	// limit 夹取到 [1,50]。
	four := make([]int64, 4)
	for i := range four {
		msg := &Message{ChannelID: f.ch1, UserID: f.userID, Content: fmt.Sprintf("限额标记第%d条", i+1)}
		if err := st.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage(limit): %v", err)
		}
		four[i] = msg.ID
	}
	if got := searchIDs(t, st, f.channelIDs, "限额标记", 2); len(got) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %v", got)
	}
	if got := searchIDs(t, st, f.channelIDs, "限额标记", 0); len(got) != 1 {
		t.Fatalf("limit=0 应夹到 1 条，实际 %v", got)
	}
	if got := searchIDs(t, st, f.channelIDs, "限额标记", 99); len(got) != 4 {
		t.Fatalf("limit=99 应夹到 50（不足 50 返回全部 4 条），实际 %v", got)
	}
}

func TestMemSearchMessages(t *testing.T) { runSearchMessages(t, NewMem()) }

func TestSQLiteSearchMessages(t *testing.T) {
	m := openTestSQLite(t)
	if err := m.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runSearchMessages(t, m)
}

func TestMySQLSearchMessages(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	var version string
	if err := m.DB().QueryRowContext(context.Background(), `SELECT VERSION()`).Scan(&version); err != nil {
		t.Fatalf("查询 MySQL 版本失败: %v", err)
	}
	t.Logf("MySQL 版本：%s（ngram FULLTEXT 需要 5.7+/8.0+，MariaDB 需 10.10+）", version)
	runSearchMessages(t, m)
}

// TestSearchMessagesNotFound 不存在的频道集合与查不到的词都返回空结果，不报错。
func TestSearchMessagesNotFound(t *testing.T) {
	st := NewMem()
	f := newSearchFixture(t, st)
	msgs, err := st.SearchMessages(context.Background(), []int64{999999}, "hello", 20)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("不存在的频道应空结果: %v %v", msgs, err)
	}
	if msgs, err := st.SearchMessages(context.Background(), f.channelIDs, "不存在的词", 20); err != nil || len(msgs) != 0 {
		t.Fatalf("查不到应空结果: %v %v", msgs, err)
	}
}
