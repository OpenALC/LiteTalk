package attachment

import (
	"context"
	"errors"
	"fmt"
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

type failReserveStore struct {
	store.Store
	failID int64
	failed bool
}

type failQueuedRowStore struct {
	store.Store
	id                      int64
	reserveFail, finishFail bool
}

func (s *failQueuedRowStore) ReserveOrphanAttachment(ctx context.Context, id int64, before time.Time) (*store.Attachment, bool, error) {
	if id == s.id && s.reserveFail {
		return nil, false, errors.New("reserve permanently failed")
	}
	return s.Store.ReserveOrphanAttachment(ctx, id, before)
}
func (s *failQueuedRowStore) FinishOrphanAttachment(ctx context.Context, id int64) (bool, error) {
	if id == s.id && s.finishFail {
		return false, errors.New("finish permanently failed")
	}
	return s.Store.FinishOrphanAttachment(ctx, id)
}
func TestGCRowRetryStageErrorStillScansNewRows(t *testing.T) {
	e := newGCEnv(t)
	first := e.attachment(t, 1725, 0, false)
	st := &failQueuedRowStore{Store: e.st, id: first.ID}
	bl := &failDeleteBlob{Store: e.bl, failKey: first.StorageKey}
	g := newExpiredGC(st, bl)
	g.Batch = 1
	if _, _, err := g.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.LastSweepStats().PendingRows != 1 {
		t.Fatal("失败行应入队")
	}
	bl.setFailKey("")
	st.reserveFail = true
	second := e.attachment(t, 1726, 0, false)
	rows, _, err := g.Sweep(context.Background())
	if err == nil || rows != 1 {
		t.Fatalf("Reserve重试失败仍需处理新行: rows=%d err=%v", rows, err)
	}
	assertAttachmentGone(t, e.st, second.ID)
	st.reserveFail = false
	st.finishFail = true
	for _, id := range []int64{1727, 1728} {
		next := e.attachment(t, id, 0, false)
		rows, _, err = g.Sweep(context.Background())
		if err == nil || rows != 1 || g.LastSweepStats().PendingRows != 1 {
			t.Fatalf("Finish重试持续失败仍需处理新行: rows=%d stats=%+v err=%v", rows, g.LastSweepStats(), err)
		}
		assertAttachmentGone(t, e.st, next.ID)
	}
	if _, err := e.st.AttachmentByID(context.Background(), first.ID); err != nil {
		t.Fatalf("失败行须保留重试: %v", err)
	}
}

func (s *failReserveStore) ReserveOrphanAttachment(ctx context.Context, id int64, before time.Time) (*store.Attachment, bool, error) {
	if id == s.failID && !s.failed {
		s.failed = true
		return nil, false, errors.New("reserve failed")
	}
	return s.Store.ReserveOrphanAttachment(ctx, id, before)
}
func TestGCReserveErrorRetriesSameCursor(t *testing.T) {
	e := newGCEnv(t)
	a := e.attachment(t, 1720, 0, false)
	st := &failReserveStore{Store: e.st, failID: a.ID}
	g := newExpiredGC(st, e.bl)
	if _, _, err := g.Sweep(context.Background()); err == nil {
		t.Fatal("Reserve 错误应返回")
	}
	rows, _, err := g.Sweep(context.Background())
	if err != nil || rows != 1 {
		t.Fatalf("错误后应从同一ID重试: %d %v", rows, err)
	}
}
func TestGCFailedHeadProgressesAcrossSweepBudget(t *testing.T) {
	e := newGCEnv(t)
	first := e.attachment(t, 1730, 0, false)
	for i := 1; i < 55; i++ {
		e.attachment(t, int64(1730+i), 0, false)
	}
	f := &failDeleteBlob{Store: e.bl, failKey: first.StorageKey}
	g := newExpiredGC(e.st, f)
	g.Batch = 1
	rows, _, err := g.Sweep(context.Background())
	if err != nil || rows != 49 {
		t.Fatalf("第一轮预算内进度: %d %v", rows, err)
	}
	rows, _, err = g.Sweep(context.Background())
	if err != nil || rows != 5 {
		t.Fatalf("跨预算续扫: %d %v", rows, err)
	}
	f.setFailKey("")
	rows, _, err = g.Sweep(context.Background())
	if err != nil || rows != 1 {
		t.Fatalf("一圈后重试失败首项: %d %v", rows, err)
	}
	assertAttachmentGone(t, e.st, first.ID)
}

func TestGCQueuedRowRetryWhileNewRowsKeepArriving(t *testing.T) {
	e := newGCEnv(t)
	first := e.attachment(t, 3000, 0, false)
	for i := 1; i < 60; i++ {
		e.attachment(t, int64(3000+i), 0, false)
	}
	f := &failDeleteBlob{Store: e.bl, failKey: first.StorageKey}
	g := newExpiredGC(e.st, f)
	g.Batch = 1
	if _, _, err := g.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.LastSweepStats().PendingRows != 1 || g.afterID == 0 {
		t.Fatalf("首轮应保留重试项并跨预算: %+v cursor=%d", g.LastSweepStats(), g.afterID)
	}
	firstCursor := g.afterID
	for i := 60; i < 110; i++ {
		e.attachment(t, int64(3000+i), 0, false)
	}
	f.setFailKey("")
	if _, _, err := g.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAttachmentGone(t, e.st, first.ID)
	if g.afterID <= firstCursor || g.LastSweepStats().RowRetriesAttempted != 1 {
		t.Fatalf("持续新增时应先重试旧行且主游标继续: %+v cursor=%d", g.LastSweepStats(), g.afterID)
	}
}

type failAllDeleteBlob struct {
	blob.Store
	fail atomic.Bool
}
type cancelDeleteBlob struct {
	blob.Store
	cancel context.CancelFunc
}

func (b *cancelDeleteBlob) Delete(ctx context.Context, key string) error {
	b.cancel()
	return ctx.Err()
}
func TestGCCancellationDoesNotQueueRow(t *testing.T) {
	e := newGCEnv(t)
	e.attachment(t, 3121, 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	g := newExpiredGC(e.st, &cancelDeleteBlob{Store: e.bl, cancel: cancel})
	_, _, err := g.Sweep(ctx)
	if !errors.Is(err, context.Canceled) || g.LastSweepStats().PendingRows != 0 {
		t.Fatalf("取消应直接返回并保留主游标: stats=%+v err=%v", g.LastSweepStats(), err)
	}
}

func (b *failAllDeleteBlob) Delete(ctx context.Context, key string) error {
	if b.fail.Load() {
		return errors.New("delete down")
	}
	return b.Store.Delete(ctx, key)
}
func TestGCRowRetryQueueOverflowIsExplicit(t *testing.T) {
	e := newGCEnv(t)
	a := e.attachment(t, 3111, 0, false)
	b := e.attachment(t, 3112, 0, false)
	f := &failAllDeleteBlob{Store: e.bl}
	f.fail.Store(true)
	g := newExpiredGC(e.st, f)
	g.Batch = 1
	g.retryLimit = 1
	if _, _, err := g.Sweep(context.Background()); err == nil || g.LastSweepStats().RetryOverflow != 1 || g.LastSweepStats().PendingRows != 1 {
		t.Fatalf("队列满必须显式失败: stats=%+v err=%v", g.LastSweepStats(), err)
	}
	f.fail.Store(false)
	if _, _, err := g.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAttachmentGone(t, e.st, a.ID)
	assertAttachmentGone(t, e.st, b.ID)
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
	mu            sync.Mutex
	objects       []blob.ObjectInfo
	failed        bool
	statHook      func(string)
	statFailKey   string
	statFails     int
	statFailAll   bool
	statPermanent map[string]bool
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
func (f *fakeScanner) Stat(ctx context.Context, key string) (blob.ObjectInfo, error) {
	if f.statPermanent[key] {
		return blob.ObjectInfo{}, errors.New("stat permanently failed")
	}
	if f.statFailAll {
		return blob.ObjectInfo{}, errors.New("stat down")
	}
	if key == f.statFailKey && f.statFails > 0 {
		f.statFails--
		return blob.ObjectInfo{}, errors.New("stat failed")
	}
	if f.statHook != nil {
		f.statHook(key)
	}
	if _, err := f.Store.Get(ctx, key); err != nil {
		return blob.ObjectInfo{}, err
	}
	for _, x := range f.objects {
		if x.Key == key {
			return x, nil
		}
	}
	return blob.ObjectInfo{}, blob.ErrNotFound
}

func TestGCThreePermanentObjectRetriesStillScanNewObjects(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	f := &fakeScanner{Store: e.bl, statPermanent: map[string]bool{"3231.bin": true, "3232.bin": true, "3233.bin": true}}
	add := func(id int) {
		key := fmt.Sprintf("%d.bin", id)
		if err := e.bl.Put(context.Background(), key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		f.objects = append(f.objects, blob.ObjectInfo{Key: key, Modified: old, Owned: true})
	}
	for id := 3231; id <= 3233; id++ {
		add(id)
	}
	g := newTestGC(e.st, f, 24*time.Hour)
	g.Batch = 1
	if _, _, err := g.SweepUntracked(context.Background(), false); err == nil || g.LastSweepStats().PendingObjects != 3 {
		t.Fatalf("三失败键应排队并止损: stats=%+v err=%v", g.LastSweepStats(), err)
	}
	g.Batch = 3 // 重试预算足以在同一轮触发三次连续错误。
	for _, id := range []int{3234, 3235} {
		add(id)
		_, deleted, err := g.SweepUntracked(context.Background(), false)
		if err == nil || deleted != 1 || g.LastSweepStats().ObjectRetriesAttempted != 3 || g.LastSweepStats().ObjectPages == 0 {
			t.Fatalf("重试阶段止损后主扫描仍应清理%d: deleted=%d stats=%+v err=%v", id, deleted, g.LastSweepStats(), err)
		}
		assertBlobGone(t, e.bl, fmt.Sprintf("%d.bin", id))
	}
}

func TestGCQueuedObjectRetryWhileNewObjectsKeepArriving(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	f := &fakeScanner{Store: e.bl, statFailKey: "3200.bin", statFails: 1}
	add := func(start, end int) {
		for i := start; i < end; i++ {
			key := fmt.Sprintf("%d.bin", i)
			if err := e.bl.Put(context.Background(), key, []byte("x"), "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
			f.objects = append(f.objects, blob.ObjectInfo{Key: key, Modified: old, Owned: true})
		}
	}
	add(3200, 3260)
	g := newTestGC(e.st, f, 24*time.Hour)
	g.Batch = 1
	if _, _, err := g.SweepUntracked(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if g.LastSweepStats().PendingObjects != 1 || g.blobCursor == "" {
		t.Fatalf("首轮应保留失败键且主扫描未到底: %+v cursor=%q", g.LastSweepStats(), g.blobCursor)
	}
	firstCursor := g.blobCursor
	add(3260, 3310)
	if _, _, err := g.SweepUntracked(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	assertBlobGone(t, e.bl, "3200.bin")
	if g.blobCursor <= firstCursor || g.LastSweepStats().ObjectRetriesAttempted != 1 {
		t.Fatalf("持续新增时应先重试旧键且主扫描继续: %+v cursor=%q", g.LastSweepStats(), g.blobCursor)
	}
}

func TestGCFreshUntrackedObjectMaturesWithContinuousListing(t *testing.T) {
	e := newGCEnv(t)
	now := time.Now()
	old := now.Add(-72 * time.Hour)
	f := &fakeScanner{Store: e.bl}
	add := func(start, end int) {
		for i := start; i < end; i++ {
			key := fmt.Sprintf("%d.bin", i)
			if err := e.bl.Put(context.Background(), key, []byte("x"), "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
			at := old
			if i == 3400 {
				at = now
			}
			f.objects = append(f.objects, blob.ObjectInfo{Key: key, Modified: at, Owned: true})
		}
	}
	add(3400, 3460)
	g := newTestGC(e.st, f, 24*time.Hour)
	g.Batch = 1
	g.now = func() time.Time { return now }
	if _, _, err := g.SweepUntracked(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	assertBlobExists(t, e.bl, "3400.bin")
	if g.LastSweepStats().PendingObjects != 1 || g.blobCursor == "" {
		t.Fatalf("新对象应等TTL并独立重试: %+v", g.LastSweepStats())
	}
	firstCursor := g.blobCursor
	add(3460, 3510)
	g.now = func() time.Time { return now.Add(72 * time.Hour) }
	if _, _, err := g.SweepUntracked(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	assertBlobGone(t, e.bl, "3400.bin")
	if g.blobCursor <= firstCursor {
		t.Fatal("主扫描仍应向前推进")
	}
}

func TestGCObjectRetryQueueOverflowIsExplicit(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	f := &fakeScanner{Store: e.bl, statFailAll: true}
	for i := 3211; i <= 3212; i++ {
		key := fmt.Sprintf("%d.bin", i)
		if err := e.bl.Put(context.Background(), key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		f.objects = append(f.objects, blob.ObjectInfo{Key: key, Modified: old, Owned: true})
	}
	g := newTestGC(e.st, f, 24*time.Hour)
	g.Batch = 1
	g.retryLimit = 1
	if _, _, err := g.SweepUntracked(context.Background(), false); err == nil || g.LastSweepStats().RetryOverflow != 1 || g.LastSweepStats().PendingObjects != 1 {
		t.Fatalf("对象队列满必须显式失败: stats=%+v err=%v", g.LastSweepStats(), err)
	}
	f.statFailAll = false
	if _, _, err := g.SweepUntracked(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	assertBlobGone(t, e.bl, "3211.bin")
	assertBlobGone(t, e.bl, "3212.bin")
}

func TestGCCancellationDoesNotQueueObject(t *testing.T) {
	e := newGCEnv(t)
	key := "3221.bin"
	if err := e.bl.Put(context.Background(), key, []byte("x"), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeScanner{Store: e.bl, objects: []blob.ObjectInfo{{Key: key, Modified: time.Now().Add(-72 * time.Hour), Owned: true}}}
	f.statHook = func(string) { cancel() }
	g := newTestGC(e.st, f, 24*time.Hour)
	_, _, err := g.SweepUntracked(ctx, false)
	if !errors.Is(err, context.Canceled) || g.LastSweepStats().PendingObjects != 0 {
		t.Fatalf("取消应直接返回并保留主游标: stats=%+v err=%v", g.LastSweepStats(), err)
	}
}

func TestGCStatFailureDoesNotStarveNextPage(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	objects := []blob.ObjectInfo{{Key: "1811.bin", Modified: old, Owned: true}, {Key: "1812.bin", Modified: old, Owned: true}}
	for _, x := range objects {
		if err := e.bl.Put(context.Background(), x.Key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeScanner{Store: e.bl, objects: objects, statFailKey: objects[0].Key, statFails: 1}
	g := newTestGC(e.st, f, 24*time.Hour)
	g.Batch = 1
	found, deleted, err := g.SweepUntracked(context.Background(), false)
	if err != nil || found != 1 || deleted != 1 || g.LastSweepStats().ObjectErrors != 1 {
		t.Fatalf("首项Stat失败: found=%d deleted=%d stats=%+v err=%v", found, deleted, g.LastSweepStats(), err)
	}
	assertBlobExists(t, e.bl, objects[0].Key)
	assertBlobGone(t, e.bl, objects[1].Key)
	found, deleted, err = g.SweepUntracked(context.Background(), false)
	if err != nil || found != 1 || deleted != 1 {
		t.Fatalf("失败项次轮重试: %d/%d %v", found, deleted, err)
	}
	assertBlobGone(t, e.bl, objects[0].Key)
}

type failKeyLookup struct {
	store.Store
	key      string
	failures int
	calls    int
}

func (s *failKeyLookup) AttachmentKeyExists(ctx context.Context, key string) (bool, error) {
	s.calls++
	if key == s.key && s.failures > 0 {
		s.failures--
		return false, errors.New("key query failed")
	}
	return s.Store.AttachmentKeyExists(ctx, key)
}
func TestGCKeyLookupFailureDoesNotStarveNextPage(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	objects := []blob.ObjectInfo{{Key: "1821.bin", Modified: old, Owned: true}, {Key: "1822.bin", Modified: old, Owned: true}}
	for _, x := range objects {
		if err := e.bl.Put(context.Background(), x.Key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeScanner{Store: e.bl, objects: objects}
	st := &failKeyLookup{Store: e.st, key: objects[0].Key, failures: 1}
	g := newTestGC(st, f, 24*time.Hour)
	g.Batch = 1
	_, deleted, err := g.SweepUntracked(context.Background(), false)
	if err != nil || deleted != 1 {
		t.Fatalf("首项DB查询失败不应阻挡后继: %d %v", deleted, err)
	}
	assertBlobGone(t, e.bl, objects[1].Key)
	_, deleted, err = g.SweepUntracked(context.Background(), false)
	if err != nil || deleted != 1 {
		t.Fatalf("失败项次轮重试: %d %v", deleted, err)
	}
	assertBlobGone(t, e.bl, objects[0].Key)
}

func TestGCGlobalKeyLookupFailureStopsAfterThree(t *testing.T) {
	e := newGCEnv(t)
	old := time.Now().Add(-72 * time.Hour)
	objects := []blob.ObjectInfo{{Key: "1831.bin", Modified: old, Owned: true}, {Key: "1832.bin", Modified: old, Owned: true}, {Key: "1833.bin", Modified: old, Owned: true}, {Key: "1834.bin", Modified: old, Owned: true}}
	for _, x := range objects {
		if err := e.bl.Put(context.Background(), x.Key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	st := &failAllKeyLookup{Store: e.st}
	g := newTestGC(st, &fakeScanner{Store: e.bl, objects: objects}, 24*time.Hour)
	g.Batch = 1
	if _, _, err := g.SweepUntracked(context.Background(), false); err == nil || st.calls != 3 {
		t.Fatalf("全局DB故障应在三次查询停止: calls=%d err=%v", st.calls, err)
	}
}

type failAllKeyLookup struct {
	store.Store
	calls int
}

func (s *failAllKeyLookup) AttachmentKeyExists(context.Context, string) (bool, error) {
	s.calls++
	return false, errors.New("db down")
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
