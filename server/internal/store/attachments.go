package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// 附件（迁移 0015，#41 B1）的双方言共用片段：列清单、插入语句与认领/校验逻辑
// 在 MySQL 与 SQLite 上完全一致（占位符均为 ?），差异只在重复键判定与时间回读。

// MaxAttachmentBatch 孤儿查询与批量删除的单批上限（Store 夹取、GC 与配置共用）。
const MaxAttachmentBatch = 500

// attachmentCols 读取列清单。message_id 为 NULL（未认领）时归零，与 Attachment.MessageID=0 同义。
const attachmentCols = `id, uploader_id, COALESCE(message_id, 0), filename, content_type, size,
	width, height, storage_key, thumb_key, access_key, sha256, created_at`

// attachmentColsAliased 与 attachmentCols 同列，带 a. 前缀：孤儿查询 join messages 时
// 两表都有 id/created_at 等列，不限定会变成歧义列。
const attachmentColsAliased = `a.id, a.uploader_id, COALESCE(a.message_id, 0), a.filename, a.content_type, a.size,
	a.width, a.height, a.storage_key, a.thumb_key, a.access_key, a.sha256, a.created_at`

// orphanAttachmentsSQL 三类孤儿，双方言语法一致（占位符均为 ?）：
//   - 未认领且超过保留期（created_at < unclaimedBefore）；
//   - 已认领但消息行已不存在（频道/分类/服务器/会话硬删后消息被级联删除）；
//   - 消息已软删。
//
// 未认领条件用 a.message_id IS NULL 而非 COALESCE，保证走 idx_attachments_message 时
// 语义直观；LIMIT ? 两端驱动都支持参数化。
const orphanAttachmentsSQL = `SELECT ` + attachmentColsAliased + `
	FROM attachments a
	LEFT JOIN messages m ON m.id = a.message_id
	WHERE a.id > ? AND ((a.message_id IS NULL AND a.created_at < ?)
	   OR (m.id IS NULL AND a.message_id IS NOT NULL)
	   OR m.deleted_at IS NOT NULL)
	ORDER BY a.id ASC
	LIMIT ?`

// clampAttachmentLimit limit 夹到 [1, MaxAttachmentBatch]。
func clampAttachmentLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > MaxAttachmentBatch {
		return MaxAttachmentBatch
	}
	return limit
}

// queryOrphanAttachments 双方言共用执行体。
func queryOrphanAttachments(ctx context.Context, db *sql.DB, unclaimedBefore time.Time, limit int) ([]Attachment, error) {
	return queryOrphanAttachmentsAfter(ctx, db, unclaimedBefore, 0, limit)
}

func queryOrphanAttachmentsAfter(ctx context.Context, db *sql.DB, unclaimedBefore time.Time, afterID int64, limit int) ([]Attachment, error) {
	limit = clampAttachmentLimit(limit)
	rows, err := db.QueryContext(ctx, orphanAttachmentsSQL, afterID, unclaimedBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Attachment, 0, limit)
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func reserveOrphanAttachment(ctx context.Context, db *sql.DB, id int64, before time.Time) (*Attachment, bool, error) {
	// 只改变已过期或已失去消息的行；在此 UPDATE 赢过消息事务前绝不删对象。
	res, err := db.ExecContext(ctx, `UPDATE attachments SET message_id = -id WHERE id = ? AND
		((message_id IS NULL AND created_at < ?) OR (message_id > 0 AND
		 (NOT EXISTS (SELECT 1 FROM messages WHERE id = attachments.message_id) OR
		  EXISTS (SELECT 1 FROM messages WHERE id = attachments.message_id AND deleted_at IS NOT NULL))))`, id, before)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n == 0 {
		// 其它 GC 实例的负标记也可幂等接手；对象键只属于此附件 ID。
		var marked int64
		err := db.QueryRowContext(ctx, `SELECT message_id FROM attachments WHERE id = ? AND message_id < 0`, id).Scan(&marked)
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	a, err := scanAttachment(db.QueryRowContext(ctx, `SELECT `+attachmentCols+` FROM attachments WHERE id = ? AND message_id < 0`, id))
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	return a, err == nil, err
}

func finishOrphanAttachment(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ? AND message_id < 0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func attachmentKeyExists(ctx context.Context, db *sql.DB, key string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM attachments WHERE storage_key = ? OR thumb_key = ? LIMIT 1`, key, key).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func pruneOrphanReadStatesAfter(ctx context.Context, db *sql.DB, afterUserID, afterChannelID int64, limit int) (nextUserID, nextChannelID int64, done bool, removed int, err error) {
	limit = clampAttachmentLimit(limit)
	rows, err := db.QueryContext(ctx, `SELECT rs.user_id, rs.channel_id, c.id FROM read_states rs LEFT JOIN channels c ON c.id = rs.channel_id WHERE rs.user_id > ? OR (rs.user_id = ? AND rs.channel_id > ?) ORDER BY rs.user_id, rs.channel_id LIMIT ?`, afterUserID, afterUserID, afterChannelID, limit)
	if err != nil {
		return 0, 0, false, 0, err
	}
	type pair struct {
		userID, channelID int64
		orphan            bool
	}
	var ids []pair
	for rows.Next() {
		var p pair
		var channel sql.NullInt64
		if err := rows.Scan(&p.userID, &p.channelID, &channel); err != nil {
			rows.Close()
			return 0, 0, false, 0, err
		}
		p.orphan = !channel.Valid
		ids = append(ids, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, false, 0, err
	}
	if len(ids) == 0 {
		return 0, 0, true, 0, nil
	}
	last := ids[len(ids)-1]
	nextUserID, nextChannelID = last.userID, last.channelID
	done = len(ids) < limit
	for _, p := range ids {
		if !p.orphan {
			continue
		}
		res, err := db.ExecContext(ctx, `DELETE FROM read_states WHERE user_id = ? AND channel_id = ? AND NOT EXISTS (SELECT 1 FROM channels WHERE id = ?)`, p.userID, p.channelID, p.channelID)
		if err != nil {
			return nextUserID, nextChannelID, done, removed, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nextUserID, nextChannelID, done, removed, err
		}
		removed += int(n)
	}
	return nextUserID, nextChannelID, done, removed, nil
}

// deleteAttachmentRowsByIDs 按批删除，单条 SQL 的参数数量不超过 MaxAttachmentBatch；
// 不存在的 id 天然被忽略，空入参 no-op。
func deleteAttachmentRowsByIDs(ctx context.Context, db *sql.DB, ids []int64) error {
	for start := 0; start < len(ids); start += MaxAttachmentBatch {
		end := start + MaxAttachmentBatch
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if _, err := db.ExecContext(ctx,
			`DELETE FROM attachments WHERE id IN (`+placeholders(len(chunk))+`)`,
			int64Args(chunk)...); err != nil {
			return err
		}
	}
	return nil
}

// attachmentInsertSQL 显式带 id 插入（Snowflake 由调用方预置，不用 AUTO_INCREMENT）。
const attachmentInsertSQL = `INSERT INTO attachments (id, uploader_id, message_id, filename, content_type,
		size, width, height, storage_key, thumb_key, access_key, sha256)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// scanAttachment 扫描 attachmentCols；未命中返回 sql.ErrNoRows，由调用方映射 ErrNotFound。
func scanAttachment(row rowScanner) (*Attachment, error) {
	var a Attachment
	if err := row.Scan(&a.ID, &a.UploaderID, &a.MessageID, &a.Filename, &a.ContentType, &a.Size,
		&a.Width, &a.Height, &a.StorageKey, &a.ThumbKey, &a.AccessKey, &a.SHA256, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// prepareAttachment 校验调用方输入（各实现写入前的共同前置）。
// 空上传者说明调用链丢了身份，直接拒绝，避免产生无法认领、无法追溯的孤儿行。
func prepareAttachment(a *Attachment) error {
	if a.UploaderID == 0 {
		return fmt.Errorf("%w: 附件上传者为空", ErrForbidden)
	}
	return nil
}

// normalizeClaimIDs 校验消息附件的认领请求：只接受正整数且不得重复（API 层也会校验，
// 这里防御直接调用 Store 的路径）。返回错误统一包 ErrAttachmentUnavailable。
func normalizeClaimIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: 非法附件 ID %d", ErrAttachmentUnavailable, id)
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("%w: 重复附件 ID %d", ErrAttachmentUnavailable, id)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// errAttachmentClaim 统一的认领失败错误（SQL 共用；Mem 实现也复用同一文案）。
func errAttachmentClaim(id int64) error {
	return fmt.Errorf("%w: 附件 %d 不存在、非本人上传或已被引用", ErrAttachmentUnavailable, id)
}

// claimAttachments 在消息事务内把未认领附件挂到 messageID。条件里的 uploader_id 与
// message_id IS NULL 是并发安全的闸门：两个请求同时认领同一附件时只有一个能改到行，
// 另一个受影响行数为 0 并触发整条消息回滚。逐条执行而不是拼 CASE，便于错误定位。
func claimAttachments(ctx context.Context, tx *sql.Tx, messageID, uploaderID int64, ids []int64) error {
	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`UPDATE attachments SET message_id = ? WHERE id = ? AND uploader_id = ? AND message_id IS NULL`,
			messageID, id, uploaderID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errAttachmentClaim(id)
		}
	}
	return nil
}
