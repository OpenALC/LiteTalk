package attachment

import (
	"context"
	"errors"
	"log"
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
	MaxGCBatch = store.MaxAttachmentBatch
)

// GC 附件生命周期回收：未认领超期、消息已硬删、消息已软删的三类孤儿。
// 删除顺序是先 blob 后元数据行——反过来一旦进程在中间退出，行没了 key 也就
// 永久无法定位对象；失败时保留行，下一轮重试（ErrNotFound 视为已删除）。
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
	now func() time.Time
}

// Run 启动即扫一轮，之后按 Interval 循环；ctx 取消后退出。
// 调用方用 `go gc.Run(ctx)`，启动路径不做同步 Sweep。
func (g *GC) Run(ctx context.Context) {
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

// Sweep 执行一轮回收：按 id 升序分批查询孤儿，逐行删 blob，全部对象都成功
// （不存在视为成功）才把该行交给 DeleteAttachmentRows；Store 查询/删行错误
// 直接返回，本轮已删除的计数仍生效。返回累计删除的行数与对象数。
func (g *GC) Sweep(ctx context.Context) (rows, blobs int, err error) {
	if g.Store == nil || g.Blob == nil {
		return 0, 0, errors.New("attachment: GC 缺少 Store 或 Blob")
	}
	batch := g.batch()
	lg := g.logger()
	for {
		list, qerr := g.Store.OrphanAttachments(ctx, g.timeNow().Add(-g.unclaimedTTL()), batch)
		if qerr != nil {
			return rows, blobs, qerr
		}
		if len(list) == 0 {
			return rows, blobs, nil
		}
		ids := make([]int64, 0, len(list))
		for i := range list {
			n, ok := g.deleteBlobs(ctx, &list[i], lg)
			blobs += n
			if ok {
				ids = append(ids, list[i].ID)
			}
		}
		// 本轮没有任何行可删（对象删除全失败）：保留行等下个周期重试。
		// 不空转重查，否则失败行会一直占满批首，形成忙循环。
		if len(ids) == 0 {
			return rows, blobs, nil
		}
		if derr := g.Store.DeleteAttachmentRows(ctx, ids); derr != nil {
			return rows, blobs, derr
		}
		rows += len(ids)
		if len(list) < batch {
			return rows, blobs, nil
		}
	}
}

// sweep 执行一轮并记录结果/错误（Run 不因单轮失败退出）。
func (g *GC) sweep(ctx context.Context) {
	rows, blobs, err := g.Sweep(ctx)
	if err != nil {
		// 关闭期间 ctx 取消属预期，不刷错误日志。
		if ctx.Err() == nil {
			g.logger().Printf("附件回收: 本轮失败: %v", err)
		}
		return
	}
	if rows > 0 || blobs > 0 {
		g.logger().Printf("附件回收: 清理 %d 行 / %d 个对象", rows, blobs)
	}
}

// deleteBlobs 删一个附件的全部对象。任一对象删除失败即停止并返回 false，
// 避免部分对象先没、行却因后续失败留下的半清理状态（重试仍幂等）。
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
