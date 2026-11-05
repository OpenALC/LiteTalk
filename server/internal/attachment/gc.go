package attachment

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 后台回收默认值与批上限（#41 生命周期回收）：
//   - 未认领附件保留 24h，给「上传后切频道/网络失败后重试」留出窗口；
//   - 每小时扫一轮，单批 200 行，避免一次锁住大表。
const (
	DefaultGCUnclaimedTTL = 24 * time.Hour
	DefaultGCInterval     = time.Hour
	DefaultGCBatch        = 200
	// MaxGCBatch 与 store 的孤儿查询夹取同值，配置解析也用它。
	MaxGCBatch           = store.MaxAttachmentBatch
	maxGCBatchesPerSweep = 50
)

// GC 回收附件行与历史无行对象，并分批清理旧 read_states 孤儿。
// 行先原子标为负 message_id，再删对象、删行；崩溃后仍可从行定位对象续扫。
type GC struct {
	Store store.Store
	Blob  blob.Store
	// UnclaimedTTL 未认领附件的保留时长；<=0 用 DefaultGCUnclaimedTTL。
	UnclaimedTTL time.Duration
	// Interval 扫描间隔；<=0 用 DefaultGCInterval。
	Interval time.Duration
	// Batch 单批行数；<=0 用 DefaultGCBatch，>MaxGCBatch 回落 MaxGCBatch。
	Batch int
	// Log 为空时用 log.Default()。
	Log *log.Logger
	// now 仅供测试注入时钟；nil 用 time.Now。
	now           func() time.Time
	sweepMu       sync.Mutex
	afterID       int64
	blobCursor    string
	readUserID    int64
	readChannelID int64
	lastStats     SweepStats
}

// SweepStats 是最近一轮后台扫描的计数，便于衡量每小时新增 I/O。
type SweepStats struct {
	AttachmentCandidates int
	ReadBatches          int
	ReadPruned           int
	ObjectPages          int
	ObjectsListed        int
	UntrackedFound       int
	UntrackedUnowned     int
}

func (g *GC) LastSweepStats() SweepStats {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	return g.lastStats
}

// Run 启动即扫一轮，之后按 Interval 循环；ctx 取消后退出。
// 调用方用 `go gc.Run(ctx)`，启动路径不做同步 Sweep。
func (g *GC) Run(ctx context.Context) {
	if scanner, ok := g.Blob.(interface{ CloseScan() error }); ok {
		defer scanner.CloseScan()
	}
	g.sweep(ctx)
	ticker := time.NewTicker(g.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweep(ctx)
		}
	}
}

// Sweep 每类至多扫描 50 批，游标跨轮保留；返回附件行与对象删除数。
func (g *GC) Sweep(ctx context.Context) (rows, blobs int, err error) {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	stats := SweepStats{}
	defer func() { g.lastStats = stats }()
	if g.Store == nil || g.Blob == nil {
		return 0, 0, errors.New("attachment: GC 缺少 Store 或 Blob")
	}
	batch := g.batch()
	lg := g.logger()
	before := g.timeNow().Add(-g.unclaimedTTL())
	for cycle := 0; cycle < maxGCBatchesPerSweep; cycle++ {
		list, qerr := g.Store.OrphanAttachmentsAfter(ctx, before, g.afterID, batch)
		if qerr != nil {
			return rows, blobs, qerr
		}
		if len(list) == 0 {
			g.afterID = 0
			break
		}
		stats.AttachmentCandidates += len(list)
		for i := range list {
			g.afterID = list[i].ID
			owned, ok, rerr := g.Store.ReserveOrphanAttachment(ctx, list[i].ID, before)
			if rerr != nil {
				return rows, blobs, rerr
			}
			if !ok {
				continue
			}
			n, ok := g.deleteBlobs(ctx, owned, lg)
			blobs += n
			if ok {
				deleted, derr := g.Store.FinishOrphanAttachment(ctx, owned.ID)
				if derr != nil {
					return rows, blobs, derr
				}
				if deleted {
					rows++
				}
			}
		}
		if len(list) < batch {
			g.afterID = 0
			break
		}
	}
	for page := 0; page < maxGCBatchesPerSweep; page++ {
		uid, cid, done, removed, perr := g.Store.PruneOrphanReadStatesAfter(ctx, g.readUserID, g.readChannelID, batch)
		if perr != nil {
			return rows, blobs, perr
		}
		stats.ReadBatches++
		stats.ReadPruned += removed
		g.readUserID, g.readChannelID = uid, cid
		if done {
			g.readUserID, g.readChannelID = 0, 0
			break
		}
	}
	_, deleted, err := g.sweepUntracked(ctx, false, &stats)
	return rows, blobs + deleted, err
}

// SweepUntracked 仅枚举无元数据行的合法附件键；dryRun 只计数不删除。
// 每轮最多 50 页，每页至多 Batch 个对象，游标跨轮保留。
func (g *GC) SweepUntracked(ctx context.Context, dryRun bool) (found, deleted int, err error) {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	stats := SweepStats{}
	defer func() { g.lastStats = stats }()
	return g.sweepUntracked(ctx, dryRun, &stats)
}

func (g *GC) sweepUntracked(ctx context.Context, dryRun bool, stats *SweepStats) (found, deleted int, err error) {
	if g.Store == nil || g.Blob == nil {
		return 0, 0, errors.New("attachment: GC 缺少 Store 或 Blob")
	}
	scanner, ok := g.Blob.(blob.Scanner)
	if !ok {
		return 0, 0, nil
	}
	before := g.timeNow().Add(-2 * g.unclaimedTTL())
	cursor := g.blobCursor
	for page := 0; page < maxGCBatchesPerSweep; page++ {
		objects, next, done, err := scanner.Scan(ctx, cursor, g.batch())
		if err != nil {
			return found, deleted, err
		}
		stats.ObjectPages++
		stats.ObjectsListed += len(objects)
		pageDeleted := 0
		for _, obj := range objects {
			if !isAttachmentKey(obj.Key) || !obj.Modified.Before(before) {
				continue
			}
			exists, err := g.Store.AttachmentKeyExists(ctx, obj.Key)
			if err != nil {
				return found, deleted, err
			}
			if exists {
				continue
			}
			current, err := scanner.Stat(ctx, obj.Key)
			if errors.Is(err, blob.ErrNotFound) {
				continue
			}
			if err != nil {
				return found, deleted, err
			}
			if !current.Modified.Equal(obj.Modified) || current.Size != obj.Size || current.ETag != obj.ETag || !current.Modified.Before(before) {
				continue
			}
			exists, err = g.Store.AttachmentKeyExists(ctx, obj.Key)
			if err != nil {
				return found, deleted, err
			}
			if exists {
				continue
			}
			found++
			stats.UntrackedFound++
			if dryRun {
				continue
			}
			if !current.Owned {
				// S3 共享桶内旧对象没有本应用标记；只报告，不按键名猜所有权。
				stats.UntrackedUnowned++
				continue
			}
			if err := g.Blob.Delete(ctx, obj.Key); err != nil && !errors.Is(err, blob.ErrNotFound) {
				g.logger().Printf("附件回收: 删除无行对象 %s 失败: %v", obj.Key, err)
				continue
			}
			deleted++
			pageDeleted++
		}
		cursor = next
		if unstable, ok := scanner.(interface{ RestartAfterDelete() bool }); ok && unstable.RestartAfterDelete() && pageDeleted > 0 {
			// 本地目录按偏移枚举；删除会移动后续位置，从头继续才不会漏项。
			cursor = ""
			if !dryRun {
				g.blobCursor = cursor
			}
			continue
		}
		if done {
			cursor = ""
			break
		}
		if !dryRun {
			g.blobCursor = cursor
		}
	}
	if !dryRun {
		g.blobCursor = cursor
	}
	return found, deleted, nil
}

func isAttachmentKey(key string) bool {
	idText := ""
	switch {
	case strings.HasSuffix(key, ".bin"):
		idText = strings.TrimSuffix(key, ".bin")
	case strings.HasSuffix(key, ".thumb.jpg"):
		idText = strings.TrimSuffix(key, ".thumb.jpg")
	default:
		return false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return false
	}
	storage, thumb := Keys(id)
	return key == storage || key == thumb
}

// sweep 执行一轮并记录结果/错误（Run 不因单轮失败退出）。
func (g *GC) sweep(ctx context.Context) {
	started := time.Now()
	rows, blobs, err := g.Sweep(ctx)
	if err != nil {
		// 关闭期间 ctx 取消属预期，不刷错误日志。
		if ctx.Err() == nil {
			g.logger().Printf("附件回收: 本轮失败: %v", err)
		}
		return
	}
	stats := g.LastSweepStats()
	g.logger().Printf("附件回收: 清理 %d 行 / %d 个对象；候选 %d 行、已读 %d 批/%d 孤儿、对象 %d 页/%d 个（无行候选 %d、归属未证 %d）；耗时 %s",
		rows, blobs, stats.AttachmentCandidates, stats.ReadBatches, stats.ReadPruned, stats.ObjectPages, stats.ObjectsListed, stats.UntrackedFound, stats.UntrackedUnowned, time.Since(started).Round(time.Millisecond))
}

// deleteBlobs 删一个附件的全部对象。任一对象删除失败即停止并返回 false，
// 行保留到下一轮；此前已删的对象下轮重复删除是幂等的（不存在视为成功）。
func (g *GC) deleteBlobs(ctx context.Context, a *store.Attachment, lg *log.Logger) (deleted int, ok bool) {
	for _, key := range []string{a.StorageKey, a.ThumbKey} {
		if key == "" {
			continue
		}
		err := g.Blob.Delete(ctx, key)
		switch {
		case err == nil:
			deleted++
		case errors.Is(err, blob.ErrNotFound):
			// 已不存在，等同删除成功。
		default:
			lg.Printf("附件回收: 删除对象 %s 失败（附件 %d）：%v；保留行下轮重试", key, a.ID, err)
			return deleted, false
		}
	}
	return deleted, true
}

func (g *GC) logger() *log.Logger {
	if g.Log != nil {
		return g.Log
	}
	return log.Default()
}

func (g *GC) timeNow() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *GC) unclaimedTTL() time.Duration {
	if g.UnclaimedTTL <= 0 {
		return DefaultGCUnclaimedTTL
	}
	return g.UnclaimedTTL
}

func (g *GC) interval() time.Duration {
	if g.Interval <= 0 {
		return DefaultGCInterval
	}
	return g.Interval
}

func (g *GC) batch() int {
	switch {
	case g.Batch <= 0:
		return DefaultGCBatch
	case g.Batch > MaxGCBatch:
		return MaxGCBatch
	default:
		return g.Batch
	}
}
