package store

import (
	"context"
	"database/sql"
	"fmt"
)

// 附件（迁移 0015，#41 B1）的双方言共用片段：列清单、插入语句与认领/校验逻辑
// 在 MySQL 与 SQLite 上完全一致（占位符均为 ?），差异只在重复键判定与时间回读。

// attachmentCols 读取列清单。message_id 为 NULL（未认领）时归零，与 Attachment.MessageID=0 同义。
const attachmentCols = `id, uploader_id, COALESCE(message_id, 0), filename, content_type, size,
	width, height, storage_key, thumb_key, access_key, sha256, created_at`

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
