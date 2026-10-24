package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"unicode/utf8"
)

// #42 B1：消息正文搜索。
//
// 方言差异只体现在索引与相关度：
//   - MySQL：迁移 0016 的 ngram FULLTEXT 索引，MATCH…AGAINST 打分，短查询回退 LIKE；
//   - SQLite：迁移 0016 的 FTS5 trigram 外部内容表，bm25 打分，短查询回退 LIKE；
//   - Mem：大小写不敏感子串匹配，无相关度（按 ID 倒序）。
//
// 索引同步：MySQL/SQLite 的全文索引由各自机制维护；SQLite 没有触发器（迁移执行器
// 不支持，见 0016 迁移注释），由本文件的 fts* 辅助函数在消息写入事务内同步。

// clampSearchLimit 把 limit 夹到 [1,50]：API 缺省 20，store 不区分「未给」。
func clampSearchLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > 50 {
		return 50
	}
	return limit
}

// likeEscaper 用 ! 作为 LIKE 转义符：MySQL 的 NO_BACKSLASH_ESCAPES 会让
// ESCAPE '\\' 变成两个字符，! 没有方言歧义。
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// likeContains 生成「包含 query」的 LIKE 模式（配合 ESCAPE '!'）。
func likeContains(query string) string {
	return "%" + likeEscaper.Replace(query) + "%"
}

// ftsPhrase 把用户输入整体包成 FTS5 短语：内部双引号双写，避免 MATCH 把
// AND/OR/NEAR/*/^ 等当成查询语法。
func ftsPhrase(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

// ftsInsertMessage 在调用方事务内写入一条 FTS 索引项。
func ftsInsertMessage(ctx context.Context, tx *sql.Tx, id int64, content string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO messages_fts(rowid, content) VALUES (?, ?)`, id, content)
	return err
}

// ftsDeleteMessage 用 FTS5 的 delete 命令移除旧词项。必须显式给出旧正文：
// 消息行可能已被清空（软删）或改写（编辑），普通 DELETE 读不回要删的词项。
func ftsDeleteMessage(ctx context.Context, tx *sql.Tx, id int64, oldContent string) error {
	if oldContent == "" {
		return nil // 空正文没有词项，索引里也不会有可命中的内容
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', ?, ?)`,
		id, oldContent)
	return err
}

// scanSearchRows 扫描「消息行 + 发送者昵称/tag」，保持 SQL 返回顺序。
func scanSearchRows(rows *sql.Rows) ([]Message, error) {
	out := []Message{}
	for rows.Next() {
		msg, err := scanMessage(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *msg)
	}
	return out, rows.Err()
}

// ---- Mem ----

// SearchMessages 大小写不敏感子串匹配；无相关度，按消息 ID 倒序。
func (m *Mem) SearchMessages(_ context.Context, channelIDs []int64, query string, limit int) ([]Message, error) {
	q := strings.TrimSpace(query)
	if len(channelIDs) == 0 || q == "" {
		return []Message{}, nil
	}
	limit = clampSearchLimit(limit)
	channels := make(map[int64]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		channels[id] = struct{}{}
	}
	needle := strings.ToLower(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Message{}
	for _, msg := range m.messages {
		if msg.DeletedAt != nil {
			continue
		}
		if _, ok := channels[msg.ChannelID]; !ok {
			continue
		}
		if !strings.Contains(strings.ToLower(msg.Content), needle) {
			continue
		}
		out = append(out, *m.messageCopyLocked(msg))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---- SQLite ----

// SearchMessages SQLite 实现：≥3 个字符走 FTS5 trigram（bm25 相关度），
// 1–2 个字符 trigram 无法命中，回退 LIKE；两者都按 ID 倒序兜底同分。
func (s *SQLite) SearchMessages(ctx context.Context, channelIDs []int64, query string, limit int) ([]Message, error) {
	q := strings.TrimSpace(query)
	if len(channelIDs) == 0 || q == "" {
		return []Message{}, nil
	}
	limit = clampSearchLimit(limit)
	var rows *sql.Rows
	var err error
	if utf8.RuneCountInString(q) >= 3 {
		args := append(int64Args(channelIDs), ftsPhrase(q), limit)
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+sqMessageCols+`, u.nickname, u.tag
			 FROM messages m
			 JOIN users u ON u.id = m.user_id
			 JOIN messages_fts ON messages_fts.rowid = m.id
			 WHERE m.channel_id IN (`+placeholders(len(channelIDs))+`)
			   AND m.deleted_at IS NULL
			   AND messages_fts MATCH ?
			 ORDER BY bm25(messages_fts), m.id DESC
			 LIMIT ?`, args...)
	} else {
		args := append(int64Args(channelIDs), likeContains(q), limit)
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+sqMessageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id IN (`+placeholders(len(channelIDs))+`)
			   AND m.deleted_at IS NULL
			   AND m.content LIKE ? ESCAPE '!'
			 ORDER BY m.id DESC
			 LIMIT ?`, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSearchRows(rows)
}

// ---- MySQL ----

// SearchMessages MySQL 实现：≥2 个字符先走 ngram FULLTEXT（MATCH 相关度），
// 再用 LIKE 复核当前正文仍是子串匹配；1 个字符 ngram 无法命中，直接 LIKE。
// 2 字符阈值依赖 ngram_token_size=2（MySQL 默认值）；若部署改成 3，2 字查询会
// 落空而不是误报，调整阈值即可。
// LIKE 复核不是冗余：ngram 的 NATURAL LANGUAGE MODE 只要任一切分片段相同就算
// 命中（查询「升级前关键词」也会命中含「升级」的另一条消息），与另外两个实现
// 的子串语义不一致；LIKE 把语义收回「当前正文包含完整查询词」。相关度相同按 ID 倒序。
func (m *MySQL) SearchMessages(ctx context.Context, channelIDs []int64, query string, limit int) ([]Message, error) {
	q := strings.TrimSpace(query)
	if len(channelIDs) == 0 || q == "" {
		return []Message{}, nil
	}
	limit = clampSearchLimit(limit)
	var rows *sql.Rows
	var err error
	if utf8.RuneCountInString(q) >= 2 {
		args := append(int64Args(channelIDs), q, likeContains(q), q, limit)
		rows, err = m.db.QueryContext(ctx,
			`SELECT `+messageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id IN (`+placeholders(len(channelIDs))+`)
			   AND m.deleted_at IS NULL
			   AND MATCH(m.content) AGAINST(? IN NATURAL LANGUAGE MODE)
			   AND m.content LIKE ? ESCAPE '!'
			 ORDER BY MATCH(m.content) AGAINST(? IN NATURAL LANGUAGE MODE) DESC, m.id DESC
			 LIMIT ?`, args...)
	} else {
		args := append(int64Args(channelIDs), likeContains(q), limit)
		rows, err = m.db.QueryContext(ctx,
			`SELECT `+messageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id IN (`+placeholders(len(channelIDs))+`)
			   AND m.deleted_at IS NULL
			   AND m.content LIKE ? ESCAPE '!'
			 ORDER BY m.id DESC
			 LIMIT ?`, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSearchRows(rows)
}
