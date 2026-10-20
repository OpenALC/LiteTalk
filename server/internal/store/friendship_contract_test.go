package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	mysqldrv "github.com/go-sql-driver/mysql"
)

// friendshipTestDB MySQL 专用好友契约库：每次用例重建，避免与同包其它用例互相污染。
const friendshipTestDB = "litetalk_friendship_test"

// runFriendshipContract 三个实现共用的好友关系契约（#76）。
// 每个子用例使用独立的用户 id 段，互不依赖执行顺序。
func runFriendshipContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("创建即写入备注与分组", func(t *testing.T) {
		f := &Friendship{UserID: 101, FriendID: 202, Status: FriendPending, Remark: "老张", GroupName: "队友"}
		if err := st.CreateFriendship(ctx, f); err != nil {
			t.Fatalf("CreateFriendship: %v", err)
		}
		if f.ID == 0 || f.CreatedAt.IsZero() {
			t.Fatalf("创建后应回填 id/created_at: %+v", f)
		}
		got, err := st.FriendshipByID(ctx, f.ID)
		if err != nil || got.Remark != "老张" || got.GroupName != "队友" {
			t.Fatalf("备注/分组未落库: %+v %v", got, err)
		}
		// 反向查询命中同一行，方向仍是发起方 A→B。
		btw, err := st.FriendshipBetween(ctx, 202, 101)
		if err != nil || btw.ID != f.ID || btw.UserID != 101 || btw.FriendID != 202 {
			t.Fatalf("反向 FriendshipBetween 不符: %+v %v", btw, err)
		}
		if btw.Remark != "老张" || btw.GroupName != "队友" {
			t.Fatalf("反向读取丢失备注/分组: %+v", btw)
		}
	})

	t.Run("反向与同向重复均冲突且只存一行", func(t *testing.T) {
		if err := st.CreateFriendship(ctx, &Friendship{UserID: 301, FriendID: 302, Status: FriendPending}); err != nil {
			t.Fatalf("CreateFriendship: %v", err)
		}
		if err := st.CreateFriendship(ctx, &Friendship{UserID: 302, FriendID: 301, Status: FriendPending}); !errors.Is(err, ErrConflict) {
			t.Fatalf("反向重复应 ErrConflict，实际 %v", err)
		}
		if err := st.CreateFriendship(ctx, &Friendship{UserID: 301, FriendID: 302, Status: FriendAccepted}); !errors.Is(err, ErrConflict) {
			t.Fatalf("同向重复应 ErrConflict，实际 %v", err)
		}
		for _, uid := range []int64{301, 302} {
			list, err := st.FriendshipsOfUser(ctx, uid)
			if err != nil {
				t.Fatalf("FriendshipsOfUser(%d): %v", uid, err)
			}
			n := 0
			for _, f := range list {
				if (f.UserID == 301 && f.FriendID == 302) || (f.UserID == 302 && f.FriendID == 301) {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("用户 %d 的好友列表应只含 1 行该关系，实际 %d: %+v", uid, n, list)
			}
		}
		ab, errA := st.FriendshipBetween(ctx, 301, 302)
		ba, errB := st.FriendshipBetween(ctx, 302, 301)
		if errA != nil || errB != nil || ab.ID != ba.ID {
			t.Fatalf("双向命中应指向同一行: %+v/%v %+v/%v", ab, errA, ba, errB)
		}
	})

	t.Run("更新备注后两方向读取一致", func(t *testing.T) {
		f := &Friendship{UserID: 401, FriendID: 402, Status: FriendPending}
		if err := st.CreateFriendship(ctx, f); err != nil {
			t.Fatalf("CreateFriendship: %v", err)
		}
		f.Status = FriendAccepted
		f.Remark = "小李"
		f.GroupName = "同学"
		if err := st.UpdateFriendship(ctx, f); err != nil {
			t.Fatalf("UpdateFriendship: %v", err)
		}
		for _, pair := range [][2]int64{{401, 402}, {402, 401}} {
			got, err := st.FriendshipBetween(ctx, pair[0], pair[1])
			if err != nil {
				t.Fatalf("FriendshipBetween(%d,%d): %v", pair[0], pair[1], err)
			}
			if got.Status != FriendAccepted || got.Remark != "小李" || got.GroupName != "同学" {
				t.Fatalf("方向 %v 读到旧值: %+v", pair, got)
			}
		}
		if got, err := st.FriendshipByID(ctx, f.ID); err != nil || got.Remark != "小李" || got.GroupName != "同学" {
			t.Fatalf("FriendshipByID 读到旧值: %+v %v", got, err)
		}
	})

	t.Run("并发反向创建恰好一个成功", func(t *testing.T) {
		cases := []*Friendship{
			{UserID: 501, FriendID: 502, Status: FriendPending},
			{UserID: 502, FriendID: 501, Status: FriendPending},
		}
		start := make(chan struct{})
		errs := make(chan error, len(cases))
		var wg sync.WaitGroup
		for _, f := range cases {
			wg.Add(1)
			go func(f *Friendship) {
				defer wg.Done()
				<-start
				errs <- st.CreateFriendship(ctx, f)
			}(f)
		}
		close(start)
		wg.Wait()
		close(errs)

		var ok, conflict int
		for err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrConflict):
				conflict++
			default:
				t.Fatalf("并发创建出现意外错误: %v", err)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("并发反向创建应恰好一成功一冲突，实际成功 %d 冲突 %d", ok, conflict)
		}
		list, err := st.FriendshipsOfUser(ctx, 501)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, f := range list {
			if (f.UserID == 501 && f.FriendID == 502) || (f.UserID == 502 && f.FriendID == 501) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("并发后该关系应只有一行，实际 %d: %+v", n, list)
		}
	})
}

func TestFriendshipContractMem(t *testing.T) {
	runFriendshipContract(t, NewMem())
}

func TestFriendshipContractSQLite(t *testing.T) {
	m := openTestSQLite(t)
	if err := m.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runFriendshipContract(t, m)
}

// openFriendshipMySQL 建一个专用、干净的 MySQL 测试库。
// DSN 里带上库名而不是靠 USE：连接池会新开连接，USE 只影响执行它的那条连接，
// 并发用例会因此落到无库连接上。
func openFriendshipMySQL(t *testing.T) *MySQL {
	t.Helper()
	base := openTestMySQL(t) // DSN 不可用时跳过
	dsn := os.Getenv("LITETALK_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true"
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		base.Close()
		t.Fatalf("解析 MySQL DSN 失败: %v", err)
	}
	ctx := context.Background()
	if _, err := base.DB().ExecContext(ctx, "DROP DATABASE IF EXISTS "+friendshipTestDB); err != nil {
		base.Close()
		t.Fatalf("清理专用测试库失败: %v", err)
	}
	if _, err := base.DB().ExecContext(ctx,
		"CREATE DATABASE "+friendshipTestDB+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		base.Close()
		t.Fatalf("建专用测试库失败: %v", err)
	}
	base.Close()

	cfg.DBName = friendshipTestDB
	// OpenMySQL 优先读环境变量；t.Setenv 让后续连接也指向专用库，用例结束自动还原。
	t.Setenv("LITETALK_MYSQL_DSN", cfg.FormatDSN())
	m, err := OpenMySQL(cfg.FormatDSN())
	if err != nil {
		t.Fatalf("连接专用测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestFriendshipContractMySQL(t *testing.T) {
	m := openFriendshipMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runFriendshipContract(t, m)
}

// migrationsBefore 复制 fsys 中版本号小于 version 的迁移文件，模拟升级前的老库。
func migrationsBefore(t *testing.T, fsys fs.FS, version string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if strings.SplitN(name, "_", 2)[0] >= version {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: body}
	}
	return out
}

// TestFriendshipPairMigrationSQLite #76：老库里的反向重复在 0008 迁移时去重，
// 之后唯一索引拒绝反向再插入。
func TestFriendshipPairMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0008")); err != nil {
		t.Fatalf("迁移到 0007 失败: %v", err)
	}
	// 预置：一对反向重复 + 一对正常数据（迁移不得误删）。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO friendships (id, user_id, friend_id, status) VALUES
		 (7001, 601, 602, 'accepted'), (7002, 602, 601, 'pending'), (7003, 603, 604, 'accepted')`); err != nil {
		t.Fatalf("预置重复数据失败: %v", err)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("应用 0008 失败: %v", err)
	}

	var ids []int64
	rows, err := m.DB().QueryContext(ctx,
		`SELECT id FROM friendships
		  WHERE (user_id = 601 AND friend_id = 602) OR (user_id = 602 AND friend_id = 601)
		  ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 7001 {
		t.Fatalf("去重应只保留最早一行 7001，实际 %v", ids)
	}
	var kept int
	if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM friendships WHERE id = 7003`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("无关关系不应被删除 cnt=%d err=%v", kept, err)
	}

	// 唯一索引拒绝反向与同向重复，Store 层映射为 ErrConflict。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO friendships (id, user_id, friend_id, status) VALUES (7004, 602, 601, 'pending')`); err == nil || !isDupSQLite(err) {
		t.Fatalf("反向重复应被唯一索引拒绝，实际 %v", err)
	}
	if err := m.CreateFriendship(ctx, &Friendship{UserID: 602, FriendID: 601, Status: FriendPending}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Store 层反向重复应 ErrConflict，实际 %v", err)
	}
}

// TestFriendshipPairMigrationMySQL #76：MySQL 函数索引方案的真库验证
// （老库去重、唯一索引生效、down 只删索引）。
func TestFriendshipPairMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openFriendshipMySQL(t)
	full := findMigrations(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0008")); err != nil {
		t.Fatalf("迁移到 0007 失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO friendships (id, user_id, friend_id, status) VALUES
		 (7101, 611, 612, 'accepted'), (7102, 612, 611, 'pending'), (7103, 613, 614, 'accepted')`); err != nil {
		t.Fatalf("预置重复数据失败: %v", err)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("应用 0008 失败: %v", err)
	}

	var ids []int64
	rows, err := m.DB().QueryContext(ctx,
		`SELECT id FROM friendships
		  WHERE (user_id = 611 AND friend_id = 612) OR (user_id = 612 AND friend_id = 611)
		  ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 7101 {
		t.Fatalf("去重应只保留最早一行 7101，实际 %v", ids)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO friendships (id, user_id, friend_id, status) VALUES (7104, 612, 611, 'pending')`); err == nil || !isDup(err) {
		t.Fatalf("反向重复应被唯一索引拒绝，实际 %v", err)
	}
	if err := m.CreateFriendship(ctx, &Friendship{UserID: 612, FriendID: 611, Status: FriendPending}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Store 层反向重复应 ErrConflict，实际 %v", err)
	}

	// down 只撤索引：回滚后索引消失，再迁移可重建（此时数据已无重复）。
	if err := m.RollbackLast(ctx, full); err != nil {
		t.Fatalf("回滚 0008 失败: %v", err)
	}
	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.statistics
		  WHERE table_schema = DATABASE() AND table_name = 'friendships' AND index_name = 'uk_friendships_unordered'`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("down 后无序唯一索引应消失，实际存在 %d 条索引记录", n)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("回滚后再迁移失败: %v", err)
	}
}
