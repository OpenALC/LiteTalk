package store

import (
	"context"
	"database/sql"
	"sort"
)

// rowScanner 是 *sql.Row / *sql.Rows 的公共 Scan 接口（消息读取共用）。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanMessage 扫描 messageCols（#39 起行尾带 edited_at/deleted_at/reply_to）。
// includeSender 为 true 时，行尾还要有 users.nickname、users.tag 两列。
// 未命中返回 sql.ErrNoRows，由调用方映射为 ErrNotFound。
func scanMessage(row rowScanner, includeSender bool) (*Message, error) {
	var msg Message
	var editedAt, deletedAt sql.NullTime
	var replyTo sql.NullInt64
	dest := []any{
		&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content, &msg.Nonce, &msg.CreatedAt,
		&editedAt, &deletedAt, &replyTo,
	}
	if includeSender {
		dest = append(dest, &msg.SenderNickname, &msg.SenderTag)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if editedAt.Valid {
		t := editedAt.Time
		msg.EditedAt = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		msg.DeletedAt = &t
	}
	if replyTo.Valid {
		id := replyTo.Int64
		msg.ReplyTo = &id
	}
	return &msg, nil
}

// normalizeMentions 去重并按 user_id 升序：message_mentions 主键是
// (message_id, user_id)，批量读取也按同一顺序返回，两边语义一致。
func normalizeMentions(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// insertMessageMentions 在调用方事务内逐条写提及；mentions 须已 normalizeMentions。
func insertMessageMentions(ctx context.Context, tx *sql.Tx, messageID int64, mentions []int64) error {
	for _, uid := range mentions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_mentions (message_id, user_id) VALUES (?, ?)`,
			messageID, uid); err != nil {
			return err
		}
	}
	return nil
}

// int64Args 把 id 列表转成 database/sql 的变参（空列表由调用方提前返回）。
func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
