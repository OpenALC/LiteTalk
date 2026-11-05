package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func runAttachmentGCContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	users := attachmentUsers(t, st, 2)
	owner := users[0]
	sv := &Server{Name: "gc", OwnerID: owner.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	ch := &Channel{ServerID: sv.ID, Name: "gc", Type: ChannelText}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	first := newTestAttachment(owner.ID)
	first.ID = newID(0)
	first.StorageKey = "gc-first.bin"
	first.ThumbKey = ""
	if err := st.CreateAttachment(ctx, first); err != nil {
		t.Fatal(err)
	}
	// MySQL DATETIME 可能沿用历史会话时区；阈值留足跨时区余量，时区修复另行核查。
	old := time.Now().Add(48 * time.Hour)
	list, err := st.OrphanAttachmentsAfter(ctx, old, 0, 1)
	if err != nil || len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("孤儿游标: %+v %v", list, err)
	}
	msg := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "x", AttachmentIDs: []int64{first.ID}}
	if err := st.CreateMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ReserveOrphanAttachment(ctx, first.ID, old); err != nil || ok {
		t.Fatalf("已认领不可回收: %v %v", ok, err)
	}
	if ok, err := st.AttachmentKeyExists(ctx, first.StorageKey); err != nil || !ok {
		t.Fatalf("有行对象键: %v %v", ok, err)
	}
	second := newTestAttachment(owner.ID)
	second.ID = newID(0)
	second.StorageKey = "gc-second.bin"
	second.ThumbKey = ""
	if err := st.CreateAttachment(ctx, second); err != nil {
		t.Fatal(err)
	}
	reserved, ok, err := st.ReserveOrphanAttachment(ctx, second.ID, old)
	if err != nil || !ok || reserved.MessageID >= 0 {
		t.Fatalf("认领回收权: %+v %v %v", reserved, ok, err)
	}
	if _, ok, err := st.ReserveOrphanAttachment(ctx, second.ID, old); err != nil || !ok {
		t.Fatalf("崩溃续扫: %v %v", ok, err)
	}
	msg2 := &Message{ChannelID: ch.ID, UserID: owner.ID, Content: "x", AttachmentIDs: []int64{second.ID}}
	if err := st.CreateMessage(ctx, msg2); !errors.Is(err, ErrAttachmentUnavailable) {
		t.Fatalf("GC 占有后不能认领: %v", err)
	}
	if deleted, err := st.FinishOrphanAttachment(ctx, first.ID); err != nil || deleted {
		t.Fatalf("已认领行不得删: %v %v", deleted, err)
	}
	if deleted, err := st.FinishOrphanAttachment(ctx, second.ID); err != nil || !deleted {
		t.Fatalf("回收完结: %v %v", deleted, err)
	}
	if deleted, err := st.FinishOrphanAttachment(ctx, second.ID); err != nil || deleted {
		t.Fatalf("重复完结幂等: %v %v", deleted, err)
	}
	if ok, err := st.AttachmentKeyExists(ctx, second.StorageKey); err != nil || ok {
		t.Fatalf("无行对象键: %v %v", ok, err)
	}
}

func TestAttachmentGCContractMem(t *testing.T) { runAttachmentGCContract(t, NewMem()) }
func TestAttachmentGCContractSQLite(t *testing.T) {
	s := openTestSQLite(t)
	if err := s.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	runAttachmentGCContract(t, s)
}
func TestAttachmentGCContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatal(err)
	}
	runAttachmentGCContract(t, m)
}

func runReadStateGCContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	users := attachmentUsers(t, st, 2)
	owner := users[0]
	sv := &Server{Name: "gc read", OwnerID: owner.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	keep := &Channel{ServerID: sv.ID, Name: "keep", Type: ChannelText}
	if err := st.CreateChannel(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadState(ctx, owner.ID, keep.ID, 1); err != nil {
		t.Fatal(err)
	}
	parent := &Channel{ServerID: sv.ID, Name: "parent", Type: ChannelText}
	if err := st.CreateChannel(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &Channel{ServerID: sv.ID, Name: "child", Type: ChannelText, ParentID: &parent.ID}
	if err := st.CreateChannel(ctx, child); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{parent.ID, child.ID} {
		if err := st.SetReadState(ctx, owner.ID, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteChannel(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{parent.ID, child.ID} {
		if _, err := st.ReadState(ctx, owner.ID, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("频道/子频道已读残留 %d: %v", id, err)
		}
	}
	cat := &Category{ServerID: sv.ID, Name: "cat"}
	if err := st.CreateCategory(ctx, cat); err != nil {
		t.Fatal(err)
	}
	catCh := &Channel{ServerID: sv.ID, Name: "cat ch", Type: ChannelText, CategoryID: &cat.ID}
	if err := st.CreateChannel(ctx, catCh); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadState(ctx, owner.ID, catCh.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteCategoryWithChannels(ctx, cat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadState(ctx, owner.ID, catCh.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("分类已读残留: %v", err)
	}
	conv := &Channel{Scope: ChannelScopeDirect}
	if err := st.CreateConversation(ctx, conv, []int64{owner.ID, users[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadState(ctx, owner.ID, conv.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteChannel(ctx, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadState(ctx, owner.ID, conv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("会话已读残留: %v", err)
	}
	if _, err := st.ReadState(ctx, owner.ID, keep.ID); err != nil {
		t.Fatalf("其它频道应保留: %v", err)
	}
	if err := st.DeleteServer(ctx, sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadState(ctx, owner.ID, keep.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("服务器已读残留: %v", err)
	}
	for _, id := range []int64{int64(970000000001), int64(970000000002)} {
		if err := st.SetReadState(ctx, owner.ID, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	uid, cid, _, n, err := st.PruneOrphanReadStatesAfter(ctx, 0, 0, 1)
	if err != nil || n != 1 {
		t.Fatalf("历史孤儿单批: %d %v", n, err)
	}
	uid, cid, _, n, err = st.PruneOrphanReadStatesAfter(ctx, uid, cid, 1)
	if err != nil || n != 1 {
		t.Fatalf("历史孤儿续批: %d %v", n, err)
	}
	_, _, done, n, err := st.PruneOrphanReadStatesAfter(ctx, uid, cid, 1)
	if err != nil || n != 0 || !done {
		t.Fatalf("历史孤儿清空: %d done=%v %v", n, done, err)
	}
	liveServer := &Server{Name: "live", OwnerID: owner.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, liveServer); err != nil {
		t.Fatal(err)
	}
	liveChannel := &Channel{ServerID: liveServer.ID, Name: "live", Type: ChannelText}
	if err := st.CreateChannel(ctx, liveChannel); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadState(ctx, owner.ID, liveChannel.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadState(ctx, owner.ID, 9223372036854775806, 1); err != nil {
		t.Fatal(err)
	}
	uid, cid, done, removed, err := st.PruneOrphanReadStatesAfter(ctx, 0, 0, 1)
	if err != nil || done || removed != 0 || uid != owner.ID || cid != liveChannel.ID {
		t.Fatalf("有效行游标: %d/%d done=%v removed=%d err=%v", uid, cid, done, removed, err)
	}
	_, _, _, removed, err = st.PruneOrphanReadStatesAfter(ctx, uid, cid, 1)
	if err != nil || removed != 1 {
		t.Fatalf("游标后历史孤儿: %d %v", removed, err)
	}
}

func TestReadStateGCContractMem(t *testing.T) { runReadStateGCContract(t, NewMem()) }
func TestReadStateGCContractSQLite(t *testing.T) {
	s := openTestSQLite(t)
	if err := s.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	runReadStateGCContract(t, s)
}
func TestReadStateGCContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatal(err)
	}
	runReadStateGCContract(t, m)
}

func TestAttachmentGCKeyIndexesSQLite(t *testing.T) {
	s := openTestSQLite(t)
	if err := s.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"idx_attachments_storage_key", "idx_attachments_thumb_key"} {
		var got string
		if err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got); err != nil || got != name {
			t.Fatalf("索引 %s: %q %v", name, got, err)
		}
	}
}

func TestAttachmentGCKeyIndexesMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"idx_attachments_storage_key", "idx_attachments_thumb_key"} {
		var count int
		if err := m.DB().QueryRow(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'attachments' AND index_name = ?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("索引 %s: %d %v", name, count, err)
		}
	}
}
