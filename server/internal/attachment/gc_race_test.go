package attachment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

type selectedOrphanStore struct {
	store.Store
	selected chan struct{}
	resume   chan struct{}
}

func TestGCFailedBatchHeadDoesNotStarveLaterRows(t *testing.T) {
	e := newGCEnv(t)
	first := e.attachment(t, 1702, 0, false)
	second := e.attachment(t, 1703, 0, false)
	failing := &failDeleteBlob{Store: e.bl, failKey: first.StorageKey}
	g := newExpiredGC(e.st, failing)
	g.Batch = 1
	rows, _, err := g.Sweep(context.Background())
	if err != nil || rows != 1 {
		t.Fatalf("失败批首不能阻挡后继: rows=%d err=%v", rows, err)
	}
	assertBlobExists(t, e.bl, first.StorageKey)
	assertAttachmentGone(t, e.st, second.ID)
	failing.setFailKey("")
	rows, _, err = g.Sweep(context.Background())
	if err != nil || rows != 1 {
		t.Fatalf("下轮重试失败行: rows=%d err=%v", rows, err)
	}
}

type blockFirstDelete struct {
	blob.Store
	entered chan struct{}
	release chan struct{}
	first   atomic.Bool
}

func (b *blockFirstDelete) Delete(ctx context.Context, key string) error {
	if b.first.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
	return b.Store.Delete(ctx, key)
}
func TestGCTwoInstancesAndInterruptedDelete(t *testing.T) {
	e := newGCEnv(t)
	a := e.attachment(t, 1710, 0, true)
	b := &blockFirstDelete{Store: e.bl, entered: make(chan struct{}), release: make(chan struct{})}
	g1 := newExpiredGC(e.st, b)
	g2 := newExpiredGC(e.st, b)
	done := make(chan struct {
		rows int
		err  error
	}, 2)
	go func() {
		rows, _, err := g1.Sweep(context.Background())
		done <- struct {
			rows int
			err  error
		}{rows, err}
	}()
	<-b.entered
	go func() {
		rows, _, err := g2.Sweep(context.Background())
		done <- struct {
			rows int
			err  error
		}{rows, err}
	}()
	second := <-done
	close(b.release)
	first := <-done
	if first.err != nil || second.err != nil || first.rows+second.rows != 1 {
		t.Fatalf("双实例完结次数: %+v %+v", first, second)
	}
	assertAttachmentGone(t, e.st, a.ID)
	assertBlobGone(t, e.bl, a.StorageKey)
	assertBlobGone(t, e.bl, a.ThumbKey)
}

func TestGCUntrackedLocalDryRunAndTTL(t *testing.T) {
	e := newGCEnv(t)
	ctx := context.Background()
	row := e.attachment(t, 1704, 0, false)
	for _, key := range []string{"1705.bin", "1706.thumb.jpg", "1707.bin", "1708.other"} {
		if err := e.bl.Put(ctx, key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-72 * time.Hour)
	for _, key := range []string{row.StorageKey, "1705.bin", "1706.thumb.jpg", "1708.other"} {
		if err := os.Chtimes(filepath.Join(e.dir, key), old, old); err != nil {
			t.Fatal(err)
		}
	}
	g := newTestGC(e.st, e.bl, 24*time.Hour)
	g.Batch = 2
	found, deleted, err := g.SweepUntracked(ctx, true)
	if err != nil || found != 2 || deleted != 0 {
		t.Fatalf("dry-run=%d/%d %v", found, deleted, err)
	}
	assertBlobExists(t, e.bl, "1705.bin")
	found, deleted, err = g.SweepUntracked(ctx, false)
	if err != nil || found != 2 || deleted != 2 {
		t.Fatalf("正式清理=%d/%d %v", found, deleted, err)
	}
	stats := g.LastSweepStats()
	if stats.ObjectPages == 0 || stats.ObjectsListed == 0 || stats.UntrackedFound != 2 {
		t.Fatalf("扫描成本计数未记录: %+v", stats)
	}
	for _, key := range []string{row.StorageKey, "1707.bin", "1708.other"} {
		assertBlobExists(t, e.bl, key)
	}
	for _, key := range []string{"1705.bin", "1706.thumb.jpg"} {
		assertBlobGone(t, e.bl, key)
	}
}

type fakeScanner struct {
	blob.Store
	mu       sync.Mutex
	objects  []blob.ObjectInfo
	failed   bool
	statHook func(string)
}

func (f *fakeScanner) Scan(_ context.Context, after string, limit int) ([]blob.ObjectInfo, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed {
		f.failed = false
		return nil, "", false, errors.New("list failed")
	}
	start := 0
	if after != "" {
		for i, x := range f.objects {
			if x.Key == after {
				start = i + 1
				break
			}
		}
	}
	end := start + limit
	if end > len(f.objects) {
		end = len(f.objects)
	}
	if end == start {
		return nil, "", true, nil
	}
	return append([]blob.ObjectInfo(nil), f.objects[start:end]...), f.objects[end-1].Key, end == len(f.objects), nil
}
func (f *fakeScanner) Stat(_ context.Context, key string) (blob.ObjectInfo, error) {
	if f.statHook != nil {
		f.statHook(key)
	}
	for _, x := range f.objects {
		if x.Key == key {
			return x, nil
		}
	}
	return blob.ObjectInfo{}, blob.ErrNotFound
}
func TestGCUntrackedPagedScannerRetryAndLateMetadata(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	objects := []blob.ObjectInfo{{Key: "1801.bin", Modified: old, Owned: true}, {Key: "1802.bin", Modified: old, Owned: true}, {Key: "1803.bin", Modified: old, Owned: true}}
	for _, x := range objects {
		if err := e.bl.Put(context.Background(), x.Key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeScanner{Store: e.bl, objects: objects, failed: true}
	g := newTestGC(e.st, f, 24*time.Hour)
	g.Batch = 1
	if _, _, err := g.SweepUntracked(context.Background(), false); err == nil {
		t.Fatal("分页错误必须可见")
	}
	// 对象枚举之后、二次 Stat 之前出现元数据行，必须保留对象。
	f.statHook = func(key string) {
		if key == "1802.bin" {
			f.statHook = nil
			a := &store.Attachment{ID: 1802, UploaderID: e.userID, Filename: "x", ContentType: "image/png", Size: 1, StorageKey: key, AccessKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SHA256: "b"}
			if err := e.st.CreateAttachment(context.Background(), a); err != nil {
				t.Fatal(err)
			}
		}
	}
	found, deleted, err := g.SweepUntracked(context.Background(), false)
	if err != nil || found != 2 || deleted != 2 {
		t.Fatalf("分页重试=%d/%d %v", found, deleted, err)
	}
	assertBlobExists(t, e.bl, "1802.bin")
}

func TestGCSharedBucketUnmarkedObjectIsReportedOnly(t *testing.T) {
	e := newGCEnv(t)
	key := "1804.bin"
	if err := e.bl.Put(context.Background(), key, []byte("x"), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	f := &fakeScanner{Store: e.bl, objects: []blob.ObjectInfo{{Key: key, Modified: time.Now().Add(-72 * time.Hour)}}}
	g := newTestGC(e.st, f, 24*time.Hour)
	found, deleted, err := g.SweepUntracked(context.Background(), false)
	if err != nil || found != 1 || deleted != 0 || g.LastSweepStats().UntrackedUnowned != 1 {
		t.Fatalf("无归属标记: found=%d deleted=%d stats=%+v err=%v", found, deleted, g.LastSweepStats(), err)
	}
	assertBlobExists(t, e.bl, key)
}

func (s *selectedOrphanStore) OrphanAttachmentsAfter(ctx context.Context, before time.Time, afterID int64, limit int) ([]store.Attachment, error) {
	list, err := s.Store.OrphanAttachmentsAfter(ctx, before, afterID, limit)
	close(s.selected)
	<-s.resume
	return list, err
}

func TestGCSelectedThenMessageClaims(t *testing.T) {
	e := newGCEnv(t)
	a := e.attachment(t, 1701, 0, false)
	barrier := &selectedOrphanStore{Store: e.st, selected: make(chan struct{}), resume: make(chan struct{})}
	g := newExpiredGC(barrier, e.bl)
	done := make(chan error, 1)
	go func() { _, _, err := g.Sweep(context.Background()); done <- err }()
	<-barrier.selected
	e.messageWithAttachment(t, a)
	close(barrier.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AttachmentByID(context.Background(), a.ID); err != nil {
		t.Fatalf("已认领行应保留: %v", err)
	}
	assertBlobExists(t, e.bl, a.StorageKey)
}
