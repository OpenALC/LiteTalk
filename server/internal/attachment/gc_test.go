package attachment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #41 生命周期回收：Mem Store + 本地临时目录 blob 覆盖三类孤儿与失败重试。

type gcEnv struct {
	st        *store.Mem
	bl        blob.Store
	userID    int64
	channelID int64
}

func newGCEnv(t *testing.T) *gcEnv {
	t.Helper()
	ctx := context.Background()
	st := store.NewMem()
	u := &store.User{Email: "gc@ex.test", Nickname: "gcuser", Tag: "0001", PasswordHash: "h"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sv := &store.Server{Name: "gc", OwnerID: u.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	ch := &store.Channel{ServerID: sv.ID, Name: "files", Type: store.ChannelText}
	if err := st.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	bl, _, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()}, discardLogger())
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	return &gcEnv{st: st, bl: bl, userID: u.ID, channelID: ch.ID}
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// attachment 写入一个带对象的附件行；messageID=0 表示未认领。
func (e *gcEnv) attachment(t *testing.T, id, messageID int64, withThumb bool) *store.Attachment {
	t.Helper()
	ctx := context.Background()
	storageKey := fmt.Sprintf("%d.bin", id)
	thumbKey := ""
	if withThumb {
		thumbKey = fmt.Sprintf("%d.thumb.jpg", id)
	}
	if err := e.bl.Put(ctx, storageKey, []byte("payload"), "image/png"); err != nil {
		t.Fatalf("blob.Put(%s): %v", storageKey, err)
	}
	if thumbKey != "" {
		if err := e.bl.Put(ctx, thumbKey, []byte("thumb"), "image/jpeg"); err != nil {
			t.Fatalf("blob.Put(%s): %v", thumbKey, err)
		}
	}
	a := &store.Attachment{
		ID:          id,
		UploaderID:  e.userID,
		MessageID:   messageID,
		Filename:    "a.png",
		ContentType: "image/png",
		Size:        int64(len("payload")),
		StorageKey:  storageKey,
		ThumbKey:    thumbKey,
		AccessKey:   fmt.Sprintf("%064d", id),
		SHA256:      strings.Repeat("a", 64),
	}
	if err := e.st.CreateAttachment(ctx, a); err != nil {
		t.Fatalf("CreateAttachment(%d): %v", id, err)
	}
	return a
}

// messageWithAttachment 建一条引用附件的新消息。
func (e *gcEnv) messageWithAttachment(t *testing.T, a *store.Attachment) *store.Message {
	t.Helper()
	msg := &store.Message{
		ChannelID: e.channelID, UserID: e.userID, Content: "带附件",
		AttachmentIDs: []int64{a.ID},
	}
	if err := e.st.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	return msg
}

func newTestGC(st store.Store, bl blob.Store, ttl time.Duration) *GC {
	return &GC{Store: st, Blob: bl, UnclaimedTTL: ttl, Interval: time.Hour, Log: discardLogger()}
}

// newExpiredGC 把时钟拨到 48h 后，确定性构造「未认领已超 TTL」，
// 不依赖真实时间流逝（Windows 时钟粒度下 1ns TTL 不可靠）。
func newExpiredGC(st store.Store, bl blob.Store) *GC {
	g := newTestGC(st, bl, 24*time.Hour)
	g.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	return g
}

func assertAttachmentGone(t *testing.T, st store.Store, id int64) {
	t.Helper()
	if _, err := st.AttachmentByID(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("附件 %d 应已删除，实际 %v", id, err)
	}
}

func assertBlobGone(t *testing.T, bl blob.Store, key string) {
	t.Helper()
	if _, err := bl.Get(context.Background(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("对象 %s 应已删除，实际 %v", key, err)
	}
}

func assertBlobExists(t *testing.T, bl blob.Store, key string) {
	t.Helper()
	if _, err := bl.Get(context.Background(), key); err != nil {
		t.Fatalf("对象 %s 应保留: %v", key, err)
	}
}

// TestGCSweepUnclaimedExpired 未认领且超过 TTL：行与 storage/thumb 对象都消失。
func TestGCSweepUnclaimedExpired(t *testing.T) {
	e := newGCEnv(t)
	a := e.attachment(t, 1001, 0, true)
	g := newExpiredGC(e.st, e.bl)

	rows, blobs, err := g.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rows != 1 || blobs != 2 {
		t.Fatalf("Sweep = %d 行 / %d 对象，期望 1 / 2", rows, blobs)
	}
	assertAttachmentGone(t, e.st, a.ID)
	assertBlobGone(t, e.bl, a.StorageKey)
	assertBlobGone(t, e.bl, a.ThumbKey)
}

// TestGCSweepUnclaimedRetained 未到期未认领与正常消息附件都保留。
func TestGCSweepUnclaimedRetained(t *testing.T) {
	e := newGCEnv(t)
	fresh := e.attachment(t, 1002, 0, false)
	claimed := e.attachment(t, 1003, 0, true)
	e.messageWithAttachment(t, claimed)
	g := newTestGC(e.st, e.bl, 24*time.Hour)

	rows, blobs, err := g.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rows != 0 || blobs != 0 {
		t.Fatalf("Sweep = %d 行 / %d 对象，期望 0 / 0", rows, blobs)
	}
	for _, a := range []*store.Attachment{fresh, claimed} {
		if _, err := e.st.AttachmentByID(context.Background(), a.ID); err != nil {
			t.Fatalf("附件 %d 应保留: %v", a.ID, err)
		}
		assertBlobExists(t, e.bl, a.StorageKey)
	}
}

// TestGCSweepDeletedMessages 软删消息与消息已硬删的附件都回收。
func TestGCSweepDeletedMessages(t *testing.T) {
	e := newGCEnv(t)
	soft := e.attachment(t, 1004, 0, false)
	msg := e.messageWithAttachment(t, soft)
	if err := e.st.SoftDeleteMessage(context.Background(), msg.ID, time.Now()); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	missing := e.attachment(t, 1005, 990000001, false)
	g := newTestGC(e.st, e.bl, 24*time.Hour)

	rows, blobs, err := g.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rows != 2 || blobs != 2 {
		t.Fatalf("Sweep = %d 行 / %d 对象，期望 2 / 2", rows, blobs)
	}
	for _, a := range []*store.Attachment{soft, missing} {
		assertAttachmentGone(t, e.st, a.ID)
		assertBlobGone(t, e.bl, a.StorageKey)
	}
}

// failDeleteBlob 包一层本地实现注入指定 key 的删除失败。
type failDeleteBlob struct {
	blob.Store
	mu      sync.Mutex
	failKey string
}

func (f *failDeleteBlob) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	fail := f.failKey != "" && key == f.failKey
	f.mu.Unlock()
	if fail {
		return errors.New("delete boom")
	}
	return f.Store.Delete(ctx, key)
}

func (f *failDeleteBlob) setFailKey(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failKey = key
}

// TestGCSweepBlobFailureRetries 对象删除失败时保留行，下一轮重试成功后才删行。
func TestGCSweepBlobFailureRetries(t *testing.T) {
	t.Run("storage 失败", func(t *testing.T) {
		e := newGCEnv(t)
		a := e.attachment(t, 1006, 0, true)
		fake := &failDeleteBlob{Store: e.bl, failKey: a.StorageKey}
		g := newExpiredGC(e.st, fake)

		rows, blobs, err := g.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if rows != 0 || blobs != 0 {
			t.Fatalf("失败轮不应删行/对象: %d / %d", rows, blobs)
		}
		if _, err := e.st.AttachmentByID(context.Background(), a.ID); err != nil {
			t.Fatalf("行应保留: %v", err)
		}
		assertBlobExists(t, e.bl, a.StorageKey)
		assertBlobExists(t, e.bl, a.ThumbKey)

		fake.setFailKey("")
		rows, blobs, err = g.Sweep(context.Background())
		if err != nil || rows != 1 || blobs != 2 {
			t.Fatalf("重试 = %d 行 / %d 对象 / %v，期望 1 / 2", rows, blobs, err)
		}
		assertAttachmentGone(t, e.st, a.ID)
		assertBlobGone(t, e.bl, a.StorageKey)
		assertBlobGone(t, e.bl, a.ThumbKey)
	})

	t.Run("thumb 失败且 storage 已删", func(t *testing.T) {
		e := newGCEnv(t)
		a := e.attachment(t, 1007, 0, true)
		fake := &failDeleteBlob{Store: e.bl, failKey: a.ThumbKey}
		g := newExpiredGC(e.st, fake)

		rows, blobs, err := g.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if rows != 0 || blobs != 1 {
			t.Fatalf("storage 应已删、行应保留: %d 行 / %d 对象", rows, blobs)
		}
		if _, err := e.st.AttachmentByID(context.Background(), a.ID); err != nil {
			t.Fatalf("行应保留: %v", err)
		}
		assertBlobGone(t, e.bl, a.StorageKey)
		assertBlobExists(t, e.bl, a.ThumbKey)

		// storage 此时已不存在；local/S3 的 Delete 都幂等返回 nil，仍按一次成功
		// 调用计数，因此两轮合计 2。关键是行在 thumb 成功后才会被删除。
		fake.setFailKey("")
		rows, blobs, err = g.Sweep(context.Background())
		if err != nil || rows != 1 || blobs != 2 {
			t.Fatalf("重试 = %d 行 / %d 对象 / %v，期望 1 / 2", rows, blobs, err)
		}
		assertAttachmentGone(t, e.st, a.ID)
		assertBlobGone(t, e.bl, a.ThumbKey)
	})
}

// TestGCSweepBatches 孤儿数超过 Batch 时循环分批直到清空。
func TestGCSweepBatches(t *testing.T) {
	e := newGCEnv(t)
	const n = 7
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		a := e.attachment(t, int64(1100+i), 0, false)
		ids = append(ids, a.ID)
	}
	g := newExpiredGC(e.st, e.bl)
	g.Batch = 3

	rows, blobs, err := g.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rows != n || blobs != n {
		t.Fatalf("Sweep = %d 行 / %d 对象，期望 %d / %d", rows, blobs, n, n)
	}
	for _, id := range ids {
		assertAttachmentGone(t, e.st, id)
	}
}

// TestGCRunStartupAndCancel 启动即扫一轮（不等 Interval），ctx 取消后退出。
func TestGCRunStartupAndCancel(t *testing.T) {
	e := newGCEnv(t)
	a := e.attachment(t, 1200, 0, false)
	g := newExpiredGC(e.st, e.bl)
	g.Interval = time.Hour // 保证只有启动轮能清掉，验证启动即扫

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := e.st.AttachmentByID(ctx, a.ID); errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("启动轮未回收孤儿附件")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 Run 未退出")
	}
	assertBlobGone(t, e.bl, a.StorageKey)
}

// TestGCDefaults 零值回落与 Batch 上限。
func TestGCDefaults(t *testing.T) {
	g := &GC{}
	if g.unclaimedTTL() != DefaultGCUnclaimedTTL || g.interval() != DefaultGCInterval || g.batch() != DefaultGCBatch {
		t.Fatalf("零值默认: ttl=%s interval=%s batch=%d", g.unclaimedTTL(), g.interval(), g.batch())
	}
	g.Batch = MaxGCBatch + 100
	if g.batch() != MaxGCBatch {
		t.Fatalf("Batch 超上限应回落 %d，实际 %d", MaxGCBatch, g.batch())
	}
	if _, _, err := g.Sweep(context.Background()); err == nil {
		t.Fatal("缺少 Store/Blob 应报错")
	}
}
