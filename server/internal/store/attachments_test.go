package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #41 B1：附件元数据与消息事务内认领（迁移 0015 attachments）。
// 本文件覆盖 MySQL/SQLite/Mem 三实现契约与双方言迁移。

// attachmentSeq 保证同一二进制内用户 email/nickname 与 access_key 唯一
// （Mem 与真库用例共用；真库每次建新库，但同一测试内仍需唯一）。
var attachmentSeq atomic.Int64

func attachmentUsers(t *testing.T, st Store, n int) []*User {
	t.Helper()
	ctx := context.Background()
	seq := attachmentSeq.Add(1)
	out := make([]*User, n)
	for i := range out {
		u := &User{
			Email:        fmt.Sprintf("attach-%d-%d@ex.test", seq, i),
			Nickname:     fmt.Sprintf("at%d-%d", seq%100000, i),
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

// attachmentChannel 建一个服务器文字频道供消息落库。
func attachmentChannel(t *testing.T, st Store, ownerID int64) *Channel {
	t.Helper()
	ctx := context.Background()
	sv := &Server{Name: "attachment", OwnerID: ownerID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	ch := &Channel{ServerID: sv.ID, Name: "files", Type: ChannelText}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	return ch
}

// attachmentAccessKey 生成 64 位 hex（CHAR(64) 长度，跨用例唯一）。
func attachmentAccessKey() string {
	return fmt.Sprintf("%032x%032x", time.Now().UnixNano(), attachmentSeq.Add(1))
}

func newTestAttachment(uploaderID int64) *Attachment {
	return &Attachment{
		UploaderID:  uploaderID,
		Filename:    "截图 1.png",
		ContentType: "image/png",
		Size:        1234,
		Width:       640,
		Height:      480,
		StorageKey:  "100.bin",
		ThumbKey:    "100.thumb.jpg",
		AccessKey:   attachmentAccessKey(),
		SHA256:      strings.Repeat("a", 64),
	}
}

func mustCreateAttachment(t *testing.T, st Store, a *Attachment) *Attachment {
	t.Helper()
	if err := st.CreateAttachment(context.Background(), a); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	return a
}

func runAttachmentsContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	us := attachmentUsers(t, st, 2)
	owner, other := us[0], us[1]
	ch := attachmentChannel(t, st, owner.ID)

	t.Run("创建与按 ID/凭证读取", func(t *testing.T) {
		a := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		if a.ID == 0 {
			t.Fatal("CreateAttachment 应回填 ID")
		}
		if a.CreatedAt.IsZero() {
			t.Fatal("CreateAttachment 应回填 CreatedAt")
		}

		got, err := st.AttachmentByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("AttachmentByID: %v", err)
		}
		if got.UploaderID != a.UploaderID || got.MessageID != 0 || got.Filename != a.Filename ||
			got.ContentType != a.ContentType || got.Size != a.Size || got.Width != a.Width ||
			got.Height != a.Height || got.StorageKey != a.StorageKey || got.ThumbKey != a.ThumbKey ||
			got.AccessKey != a.AccessKey || got.SHA256 != a.SHA256 {
			t.Fatalf("按 ID 读回字段不符:\n got=%+v\nwant=%+v", got, a)
		}

		got, err = st.AttachmentByAccessKey(ctx, a.AccessKey)
		if err != nil {
			t.Fatalf("AttachmentByAccessKey: %v", err)
		}
		if got.ID != a.ID {
			t.Fatalf("按凭证读回 ID = %d，期望 %d", got.ID, a.ID)
		}

		if _, err := st.AttachmentByID(ctx, 999999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的 ID 应 ErrNotFound，实际 %v", err)
		}
		if _, err := st.AttachmentByAccessKey(ctx, strings.Repeat("f", 64)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的凭证应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("上传者与唯一约束", func(t *testing.T) {
		empty := newTestAttachment(0)
		if err := st.CreateAttachment(ctx, empty); !errors.Is(err, ErrForbidden) {
			t.Fatalf("空上传者应 ErrForbidden，实际 %v", err)
		}

		base := mustCreateAttachment(t, st, newTestAttachment(owner.ID))

		dupKey := newTestAttachment(other.ID)
		dupKey.AccessKey = base.AccessKey
		if err := st.CreateAttachment(ctx, dupKey); !errors.Is(err, ErrConflict) {
			t.Fatalf("重复 access_key 应 ErrConflict，实际 %v", err)
		}

		dupID := newTestAttachment(other.ID)
		dupID.ID = base.ID
		if err := st.CreateAttachment(ctx, dupID); !errors.Is(err, ErrConflict) {
			t.Fatalf("重复 id 应 ErrConflict，实际 %v", err)
		}

		// 显式预置的 ID 应原样使用（测试夹具/迁移场景）。
		fixed := newTestAttachment(owner.ID)
		fixed.ID = 700001
		mustCreateAttachment(t, st, fixed)
		if got, err := st.AttachmentByID(ctx, 700001); err != nil || got.ID != 700001 {
			t.Fatalf("预置 ID 未生效: %+v %v", got, err)
		}
	})

	t.Run("消息认领与批量读取", func(t *testing.T) {
		a1 := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		a2 := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		msg := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "带附件",
			AttachmentIDs: []int64{a2.ID, a1.ID}, // 乱序提交，读取按 id 升序
		}
		if err := st.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
		for _, id := range []int64{a1.ID, a2.ID} {
			got, err := st.AttachmentByID(ctx, id)
			if err != nil {
				t.Fatalf("AttachmentByID(%d): %v", id, err)
			}
			if got.MessageID != msg.ID {
				t.Fatalf("附件 %d 的 message_id = %d，期望 %d", id, got.MessageID, msg.ID)
			}
		}

		grouped, err := st.AttachmentsByMessageIDs(ctx, []int64{msg.ID, 999999999})
		if err != nil {
			t.Fatalf("AttachmentsByMessageIDs: %v", err)
		}
		list := grouped[msg.ID]
		if len(list) != 2 {
			t.Fatalf("消息附件数 = %d，期望 2（%+v）", len(list), grouped)
		}
		if list[0].ID >= list[1].ID {
			t.Fatalf("组内应按 id 升序: %d, %d", list[0].ID, list[1].ID)
		}
		if _, ok := grouped[999999999]; ok {
			t.Fatalf("不存在的消息不应出现在结果: %+v", grouped)
		}

		if empty, err := st.AttachmentsByMessageIDs(ctx, nil); err != nil || len(empty) != 0 {
			t.Fatalf("空入参应返回空 map: %+v %v", empty, err)
		}
	})

	t.Run("非本人上传拒绝且消息不落库", func(t *testing.T) {
		a := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		msg := &Message{
			ChannelID: ch.ID, UserID: other.ID, Content: "偷附件",
			Nonce:         fmt.Sprintf("steal-%d", attachmentSeq.Add(1)),
			AttachmentIDs: []int64{a.ID},
		}
		if err := st.CreateMessage(ctx, msg); !errors.Is(err, ErrAttachmentUnavailable) {
			t.Fatalf("非本人认领应 ErrAttachmentUnavailable，实际 %v", err)
		}
		if _, err := st.MessageByNonce(ctx, ch.ID, other.ID, msg.Nonce); !errors.Is(err, ErrNotFound) {
			t.Fatalf("认领失败的消息不应落库，实际 %v", err)
		}
		got, err := st.AttachmentByID(ctx, a.ID)
		if err != nil || got.MessageID != 0 {
			t.Fatalf("失败后附件应仍未认领: %+v %v", got, err)
		}

		// 本人认领成功。
		ok := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "正常", AttachmentIDs: []int64{a.ID}}
		if err := st.CreateMessage(ctx, ok); err != nil {
			t.Fatalf("本人认领应成功: %v", err)
		}
		if got, _ := st.AttachmentByID(ctx, a.ID); got == nil || got.MessageID != ok.ID {
			t.Fatalf("附件应归属成功消息: %+v", got)
		}

		// 已被引用的附件不能再次认领（同一上传者也不行）。
		again := &Message{
			ChannelID: ch.ID, UserID: owner.ID, Content: "重复引用",
			Nonce:         fmt.Sprintf("again-%d", attachmentSeq.Add(1)),
			AttachmentIDs: []int64{a.ID},
		}
		if err := st.CreateMessage(ctx, again); !errors.Is(err, ErrAttachmentUnavailable) {
			t.Fatalf("重复认领应 ErrAttachmentUnavailable，实际 %v", err)
		}
		if _, err := st.MessageByNonce(ctx, ch.ID, owner.ID, again.Nonce); !errors.Is(err, ErrNotFound) {
			t.Fatalf("重复认领的消息不应落库，实际 %v", err)
		}
		// 原归属关系不变。
		if got, _ := st.AttachmentByID(ctx, a.ID); got == nil || got.MessageID != ok.ID {
			t.Fatalf("重复认领不应改动原归属: %+v", got)
		}
	})

	t.Run("重复与非法附件 ID 拒绝", func(t *testing.T) {
		a := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		cases := map[string][]int64{
			"重复":   {a.ID, a.ID},
			"负数":   {-1},
			"零":    {0},
			"不存在":  {999999999},
			"混合失败": {a.ID, 999999999},
		}
		for name, ids := range cases {
			msg := &Message{
				ChannelID: ch.ID, UserID: owner.ID, Content: name,
				Nonce:         fmt.Sprintf("bad-%s-%d", name, attachmentSeq.Add(1)),
				AttachmentIDs: ids,
			}
			if err := st.CreateMessage(ctx, msg); !errors.Is(err, ErrAttachmentUnavailable) {
				t.Fatalf("%s：应 ErrAttachmentUnavailable，实际 %v", name, err)
			}
			if _, err := st.MessageByNonce(ctx, ch.ID, owner.ID, msg.Nonce); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s：失败消息不应落库，实际 %v", name, err)
			}
			// 有效附件也不能被半个事务认领。
			if got, err := st.AttachmentByID(ctx, a.ID); err != nil || got.MessageID != 0 {
				t.Fatalf("%s：附件应仍未认领: %+v %v", name, got, err)
			}
		}
	})

	t.Run("孤儿查询与批量删除", func(t *testing.T) {
		// 阈值取一小时前：之前子用例刚创建的未认领附件都不算超期，
		// 只有显式回填过 created_at 的旧行才算，候选集合可控。
		threshold := time.Now().Add(-time.Hour)
		oldUnclaimed := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		backdateAttachment(t, st, oldUnclaimed.ID, time.Now().Add(-2*time.Hour))
		freshUnclaimed := mustCreateAttachment(t, st, newTestAttachment(owner.ID))

		normal := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		normalMsg := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "正常附件", AttachmentIDs: []int64{normal.ID}}
		if err := st.CreateMessage(ctx, normalMsg); err != nil {
			t.Fatalf("CreateMessage(normal): %v", err)
		}

		soft := mustCreateAttachment(t, st, newTestAttachment(owner.ID))
		softMsg := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "待软删附件", AttachmentIDs: []int64{soft.ID}}
		if err := st.CreateMessage(ctx, softMsg); err != nil {
			t.Fatalf("CreateMessage(soft): %v", err)
		}
		if err := st.SoftDeleteMessage(ctx, softMsg.ID, time.Now()); err != nil {
			t.Fatalf("SoftDeleteMessage: %v", err)
		}

		// 直接构造指向不存在消息的附件：硬删频道/服务器后就是这种残留形态。
		missing := newTestAttachment(owner.ID)
		missing.MessageID = 900000000 + attachmentSeq.Add(1)
		mustCreateAttachment(t, st, missing)

		got, err := st.OrphanAttachments(ctx, threshold, 500)
		if err != nil {
			t.Fatalf("OrphanAttachments: %v", err)
		}
		want := map[int64]bool{oldUnclaimed.ID: true, soft.ID: true, missing.ID: true}
		for i, a := range got {
			if i > 0 && got[i-1].ID >= a.ID {
				t.Fatalf("结果应按 id 升序: %d, %d", got[i-1].ID, a.ID)
			}
			if !want[a.ID] {
				t.Fatalf("出现不该回收的附件 %d（正常消息/未到期未认领/历史用例残留）", a.ID)
			}
			delete(want, a.ID)
		}
		if len(want) != 0 {
			t.Fatalf("以下孤儿未返回: %v", want)
		}
		for _, id := range []int64{freshUnclaimed.ID, normal.ID} {
			for _, a := range got {
				if a.ID == id {
					t.Fatalf("附件 %d 不应被回收", id)
				}
			}
		}

		// limit 夹取：0/负数夹到 1，超大值夹到 500（候选不足 500 时全量返回）。
		one, err := st.OrphanAttachments(ctx, threshold, 0)
		if err != nil || len(one) != 1 || one[0].ID != got[0].ID {
			t.Fatalf("limit=0 应夹到 1 且取最小 id: %+v %v", one, err)
		}
		neg, err := st.OrphanAttachments(ctx, threshold, -5)
		if err != nil || len(neg) != 1 || neg[0].ID != got[0].ID {
			t.Fatalf("limit<0 应夹到 1: %+v %v", neg, err)
		}
		all, err := st.OrphanAttachments(ctx, threshold, 10000)
		if err != nil || len(all) != len(got) {
			t.Fatalf("limit 超上限应收敛到候选数: %d %v", len(all), err)
		}

		// 批量删行：命中的消失，未命中的保留；空入参/不存在 id 不报错。
		if err := st.DeleteAttachmentRows(ctx, []int64{oldUnclaimed.ID, soft.ID}); err != nil {
			t.Fatalf("DeleteAttachmentRows: %v", err)
		}
		for _, id := range []int64{oldUnclaimed.ID, soft.ID} {
			if _, err := st.AttachmentByID(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("附件 %d 应已删除，实际 %v", id, err)
			}
		}
		if _, err := st.AttachmentByID(ctx, missing.ID); err != nil {
			t.Fatalf("未列入删除的附件应保留: %v", err)
		}
		if err := st.DeleteAttachmentRows(ctx, nil); err != nil {
			t.Fatalf("空入参应 no-op: %v", err)
		}
		if err := st.DeleteAttachmentRows(ctx, []int64{missing.ID, 999999999}); err != nil {
			t.Fatalf("不存在的 id 应忽略: %v", err)
		}
	})
}

// backdateAttachment 把附件 created_at 改到过去。Store 没有修改该字段的公开入口，
// 测试按实现类型直接回填（Mem 改内存，SQL 执行 UPDATE）。
func backdateAttachment(t *testing.T, st Store, id int64, at time.Time) {
	t.Helper()
	switch s := st.(type) {
	case *Mem:
		s.mu.Lock()
		defer s.mu.Unlock()
		a, ok := s.attachments[id]
		if !ok {
			t.Fatalf("backdateAttachment: 附件 %d 不存在", id)
		}
		a.CreatedAt = at
	case *SQLite:
		if _, err := s.DB().ExecContext(context.Background(),
			`UPDATE attachments SET created_at = ? WHERE id = ?`, at.UTC(), id); err != nil {
			t.Fatalf("回填 SQLite created_at: %v", err)
		}
	case *MySQL:
		if _, err := s.DB().ExecContext(context.Background(),
			`UPDATE attachments SET created_at = ? WHERE id = ?`, at.UTC(), id); err != nil {
			t.Fatalf("回填 MySQL created_at: %v", err)
		}
	default:
		t.Fatalf("backdateAttachment: 未知 Store 类型 %T", st)
	}
}

func TestAttachmentsContractMem(t *testing.T) {
	runAttachmentsContract(t, NewMem())
}

func TestAttachmentsContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runAttachmentsContract(t, sq)
}

func TestAttachmentsContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runAttachmentsContract(t, m)
}

// ---- 0015 迁移（双方言） ----

func TestAttachmentsMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	before := migrationsBefore(t, full, "0015")
	through := migrationsBefore(t, full, "0016")
	if err := m.Migrate(ctx, before); err != nil {
		t.Fatalf("迁移到 0014 失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO attachments (id, uploader_id, filename, content_type, size, storage_key, access_key, sha256)
		 VALUES (1, 2, 'a', 'text/plain', 1, 'k', 'x', 'y')`); err == nil {
		t.Fatal("0015 之前不应存在 attachments 表")
	}

	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("应用 0015 失败: %v", err)
	}
	assertTableExistsSQLite(t, m.DB(), "attachments")
	for _, idx := range []string{"uk_attachments_access", "idx_attachments_message", "idx_attachments_uploader"} {
		var n int
		if err := m.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n == 0 {
			t.Fatalf("up 后索引 %s 应存在: %d %v", idx, n, err)
		}
	}

	insert := func(id int64, accessKey, sha string, messageID any) error {
		_, err := m.DB().ExecContext(ctx,
			`INSERT INTO attachments (id, uploader_id, message_id, filename, content_type, size,
			     width, height, storage_key, thumb_key, access_key, sha256)
			 VALUES (?, 11, ?, 'a.png', 'image/png', 10, 2, 3, 'sk', '', ?, ?)`,
			id, messageID, accessKey, sha)
		return err
	}
	if err := insert(5001, strings.Repeat("a", 64), strings.Repeat("b", 64), nil); err != nil {
		t.Fatalf("插入未认领附件失败: %v", err)
	}
	if err := insert(5001, strings.Repeat("c", 64), strings.Repeat("d", 64), nil); err == nil {
		t.Fatal("重复 id 应被主键拒绝")
	}
	if err := insert(5002, strings.Repeat("a", 64), strings.Repeat("e", 64), nil); err == nil {
		t.Fatal("重复 access_key 应被唯一索引拒绝")
	}
	if err := insert(5003, strings.Repeat("f", 64), strings.Repeat("0", 64), int64(9001)); err != nil {
		t.Fatalf("插入已认领附件失败: %v", err)
	}

	if err := m.RollbackLast(ctx, through); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'attachments'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down 后 attachments 应消失: %d %v", n, err)
	}

	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertTableExistsSQLite(t, m.DB(), "attachments")
	if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("再 up 后应为空表: %d %v", n, err)
	}
}

func assertTableExistsSQLite(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
		t.Fatalf("表 %s 应存在: %d %v", table, n, err)
	}
}

// TestAttachmentsMigrationMySQL 0015 up/down/up（DSN 不可用时跳过）。
func TestAttachmentsMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	before := migrationsBefore(t, full, "0015")
	through := migrationsBefore(t, full, "0016")
	if err := m.Migrate(ctx, before); err != nil {
		t.Fatalf("迁移到 0014 失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO attachments (id, uploader_id, filename, content_type, size, storage_key, access_key, sha256)
		 VALUES (1, 2, 'a', 'text/plain', 1, 'k', 'x', 'y')`); err == nil {
		t.Fatal("0015 之前不应存在 attachments 表")
	}

	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("应用 0015 失败: %v", err)
	}
	for _, idx := range []struct {
		name   string
		unique bool
	}{
		{"uk_attachments_access", true},
		{"idx_attachments_message", false},
		{"idx_attachments_uploader", false},
	} {
		var n int
		if err := m.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.statistics
			  WHERE table_schema = DATABASE() AND table_name = 'attachments'
			    AND index_name = ? AND non_unique = ?`, idx.name, !idx.unique).Scan(&n); err != nil || n == 0 {
			t.Fatalf("up 后索引 %s 应存在: %d %v", idx.name, n, err)
		}
	}

	insert := func(id int64, accessKey, sha string, messageID any) error {
		_, err := m.DB().ExecContext(ctx,
			`INSERT INTO attachments (id, uploader_id, message_id, filename, content_type, size,
			     width, height, storage_key, thumb_key, access_key, sha256)
			 VALUES (?, 11, ?, 'a.png', 'image/png', 10, 2, 3, 'sk', '', ?, ?)`,
			id, messageID, accessKey, sha)
		return err
	}
	if err := insert(5001, strings.Repeat("a", 64), strings.Repeat("b", 64), nil); err != nil {
		t.Fatalf("插入未认领附件失败: %v", err)
	}
	if err := insert(5001, strings.Repeat("c", 64), strings.Repeat("d", 64), nil); err == nil {
		t.Fatal("重复 id 应被主键拒绝")
	}
	if err := insert(5002, strings.Repeat("a", 64), strings.Repeat("e", 64), nil); err == nil {
		t.Fatal("重复 access_key 应被唯一索引拒绝")
	}
	// CHAR(64) ascii_bin：大小写变体不是同一凭证，不应撞唯一键。
	if err := insert(5003, strings.ToUpper(strings.Repeat("a", 64)), strings.Repeat("f", 64), nil); err != nil {
		t.Fatalf("大小写不同的 access_key 应可插入: %v", err)
	}

	if err := m.RollbackLast(ctx, through); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = 'attachments'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down 后 attachments 应消失: %d %v", n, err)
	}

	if err := m.Migrate(ctx, through); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("再 up 后应为空表: %d %v", n, err)
	}
}
