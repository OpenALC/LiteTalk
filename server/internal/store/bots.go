// bots.go bot 账号过滤查询（#51）：好友候选/@提及候选/成员管理列表的统一辅助，
// 三个后端各一份实现——查询口径只有这一处，调用方不得自行拼 is_bot 条件。
package store

import (
	"context"
	"database/sql"
)

// BotsAmong MySQL 实现：按主键 IN 一次取回。入参规模 = 成员列表规模（服务器成员
// 数量级），与 MessagesByIDs 等既有批量查询同口径，不分块。
func (m *MySQL) BotsAmong(ctx context.Context, uids []int64) (map[int64]bool, error) {
	if len(uids) == 0 {
		return map[int64]bool{}, nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id FROM users WHERE is_bot = 1 AND id IN (`+placeholders(len(uids))+`)`,
		int64Args(uids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectBots(rows)
}

// BotsAmong SQLite 实现：同 MySQL 口径。
func (s *SQLite) BotsAmong(ctx context.Context, uids []int64) (map[int64]bool, error) {
	if len(uids) == 0 {
		return map[int64]bool{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM users WHERE is_bot = 1 AND id IN (`+placeholders(len(uids))+`)`,
		int64Args(uids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectBots(rows)
}

// BotsAmong 内存实现：遍历用户表。
func (m *Mem) BotsAmong(_ context.Context, uids []int64) (map[int64]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64]bool)
	for _, id := range uids {
		if u, ok := m.users[id]; ok && u.IsBot {
			out[id] = true
		}
	}
	return out, nil
}

// collectBots 聚合查询行。
func collectBots(rows *sql.Rows) (map[int64]bool, error) {
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
