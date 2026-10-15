package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// MySQL 是 Store 的 MySQL 实现（database/sql，SQL 全参数化）。
type MySQL struct {
	db *sql.DB
}

// OpenMySQL 连接 MySQL。dsn 形如 user:pass@tcp(127.0.0.1:3306)/litetalk?parseTime=true。
// 环境变量 LITETALK_MYSQL_DSN 可覆盖。
func OpenMySQL(dsn string) (*MySQL, error) {
	if v := os.Getenv("LITETALK_MYSQL_DSN"); v != "" {
		dsn = v
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql ping: %w", err)
	}
	return &MySQL{db: db}, nil
}

// Close 关闭连接池。
func (m *MySQL) Close() error { return m.db.Close() }

// DB 暴露底层句柄（迁移工具用）。
func (m *MySQL) DB() *sql.DB { return m.db }

// Migrate 按文件名顺序执行 migrationsDir 下的 *.up.sql（golang-migrate 命名约定），
// 已执行过的跳过（记录在 schema_migrations）。
func (m *MySQL) Migrate(ctx context.Context, migrationsDir string) error {
	if _, err := m.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(255) PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return err
	}
	var ups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)
	for _, name := range ups {
		version := strings.TrimSuffix(name, ".up.sql")
		var n int
		if err := m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return err
		}
		// MySQL 驱动不支持多语句 Exec，逐条执行。
		for _, stmt := range splitSQL(string(body)) {
			if _, err := m.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
		}
		if _, err := m.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return err
		}
	}
	return nil
}

// RollbackLast 执行最近一个已应用版本的 *.down.sql（验证 down 迁移用）。
func (m *MySQL) RollbackLast(ctx context.Context, migrationsDir string) error {
	var version string
	err := m.db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(migrationsDir, version+".down.sql"))
	if err != nil {
		return err
	}
	for _, stmt := range splitSQL(string(body)) {
		if _, err := m.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rollback %s: %w", version, err)
		}
	}
	_, err = m.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, version)
	return err
}

// splitSQL 按行分割简单 SQL 脚本（迁移文件不含存储过程/触发器，按分号切分即可）。
func splitSQL(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ";") {
		stmt := strings.TrimSpace(part)
		// 忽略空段与纯注释段。
		lines := strings.Split(stmt, "\n")
		var kept []string
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "--") {
				continue
			}
			kept = append(kept, l)
		}
		if len(kept) > 0 {
			out = append(out, strings.Join(kept, "\n"))
		}
	}
	return out
}

// ---- users ----

func (m *MySQL) CreateUser(ctx context.Context, u *User) error {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO users (email, nickname, tag, password_hash, email_verified, is_guest, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.Email, u.Nickname, u.Tag, u.PasswordHash, u.EmailVerified, u.IsGuest, u.Status)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	u.ID = id
	return m.db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM users WHERE id = ?`, id).
		Scan(&u.CreatedAt, &u.UpdatedAt)
}

func (m *MySQL) scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Nickname, &u.Tag, &u.PasswordHash,
		&u.EmailVerified, &u.IsGuest, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

const userCols = `id, email, nickname, tag, password_hash, email_verified, is_guest, status, created_at, updated_at`

func (m *MySQL) UserByID(ctx context.Context, id int64) (*User, error) {
	return m.scanUser(m.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (m *MySQL) UserByEmail(ctx context.Context, email string) (*User, error) {
	return m.scanUser(m.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

func (m *MySQL) UserByNicknameTag(ctx context.Context, nickname, tag string) (*User, error) {
	return m.scanUser(m.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE nickname = ? AND tag = ?`, nickname, tag))
}

func (m *MySQL) UpdateUser(ctx context.Context, u *User) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE users SET nickname = ?, tag = ?, password_hash = ?, email_verified = ?, status = ?
		 WHERE id = ?`, u.Nickname, u.Tag, u.PasswordHash, u.EmailVerified, u.Status, u.ID)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return checkAffected(res)
}

// ---- friendships ----

func (m *MySQL) CreateFriendship(ctx context.Context, f *Friendship) error {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO friendships (user_id, friend_id, status) VALUES (?, ?, ?)`,
		f.UserID, f.FriendID, f.Status)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	f.ID = id
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM friendships WHERE id = ?`, id).
		Scan(&f.CreatedAt)
}

const friendshipCols = `id, user_id, friend_id, status, remark, group_name, created_at`

func (m *MySQL) FriendshipByID(ctx context.Context, id int64) (*Friendship, error) {
	var f Friendship
	err := m.db.QueryRowContext(ctx,
		`SELECT `+friendshipCols+` FROM friendships WHERE id = ?`, id).
		Scan(&f.ID, &f.UserID, &f.FriendID, &f.Status, &f.Remark, &f.GroupName, &f.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (m *MySQL) FriendshipsOfUser(ctx context.Context, userID int64) ([]Friendship, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+friendshipCols+` FROM friendships WHERE user_id = ? OR friend_id = ?`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Friendship{}
	for rows.Next() {
		var f Friendship
		if err := rows.Scan(&f.ID, &f.UserID, &f.FriendID, &f.Status, &f.Remark, &f.GroupName, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (m *MySQL) FriendshipBetween(ctx context.Context, a, b int64) (*Friendship, error) {
	var f Friendship
	err := m.db.QueryRowContext(ctx,
		`SELECT `+friendshipCols+` FROM friendships
		 WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)`, a, b, b, a).
		Scan(&f.ID, &f.UserID, &f.FriendID, &f.Status, &f.Remark, &f.GroupName, &f.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (m *MySQL) UpdateFriendship(ctx context.Context, f *Friendship) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE friendships SET status = ?, remark = ?, group_name = ? WHERE id = ?`,
		f.Status, f.Remark, f.GroupName, f.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (m *MySQL) DeleteFriendship(ctx context.Context, id int64) error {
	res, err := m.db.ExecContext(ctx, `DELETE FROM friendships WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// ---- servers ----

func (m *MySQL) CreateServer(ctx context.Context, s *Server) error {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO servers (name, owner_id, icon_name, icon_color) VALUES (?, ?, ?, ?)`,
		s.Name, s.OwnerID, s.IconName, s.IconColor)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	s.ID = id
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM servers WHERE id = ?`, id).
		Scan(&s.CreatedAt)
}

const serverCols = `id, name, owner_id, icon_name, icon_color, created_at`

func (m *MySQL) ServerByID(ctx context.Context, id int64) (*Server, error) {
	var s Server
	err := m.db.QueryRowContext(ctx, `SELECT `+serverCols+` FROM servers WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.OwnerID, &s.IconName, &s.IconColor, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *MySQL) ServersOfUser(ctx context.Context, userID int64) ([]Server, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.owner_id, s.icon_name, s.icon_color, s.created_at
		 FROM servers s JOIN server_members m ON m.server_id = s.id
		 WHERE m.user_id = ? ORDER BY s.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		var s Server
		if err := rows.Scan(&s.ID, &s.Name, &s.OwnerID, &s.IconName, &s.IconColor, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (m *MySQL) UpdateServer(ctx context.Context, s *Server) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE servers SET name = ?, icon_name = ?, icon_color = ? WHERE id = ?`,
		s.Name, s.IconName, s.IconColor, s.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (m *MySQL) DeleteServer(ctx context.Context, id int64) error {
	// 子表清理：channels/messages/invites/members 一并删除（无 FK 约束，显式清理）。
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id IN (SELECT id FROM channels WHERE server_id = ?)`, id); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM channels WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM categories WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM invites WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM server_members WHERE server_id = ?`, id); err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (m *MySQL) AddMember(ctx context.Context, mem *ServerMember) error {
	if mem.JoinedAt.IsZero() {
		mem.JoinedAt = time.Now()
	}
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO server_members (server_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`,
		mem.ServerID, mem.UserID, mem.Role, mem.JoinedAt)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (m *MySQL) RemoveMember(ctx context.Context, serverID, userID int64) error {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM server_members WHERE server_id = ? AND user_id = ?`, serverID, userID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

const memberJoin = `SELECT m.server_id, m.user_id, m.role, m.joined_at, u.nickname, u.tag
	FROM server_members m JOIN users u ON u.id = m.user_id`

func (m *MySQL) Member(ctx context.Context, serverID, userID int64) (*ServerMember, error) {
	var mem ServerMember
	err := m.db.QueryRowContext(ctx, memberJoin+` WHERE m.server_id = ? AND m.user_id = ?`, serverID, userID).
		Scan(&mem.ServerID, &mem.UserID, &mem.Role, &mem.JoinedAt, &mem.Nickname, &mem.Tag)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &mem, nil
}

func (m *MySQL) MembersOfServer(ctx context.Context, serverID int64) ([]ServerMember, error) {
	rows, err := m.db.QueryContext(ctx, memberJoin+` WHERE m.server_id = ? ORDER BY m.joined_at`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServerMember{}
	for rows.Next() {
		var mem ServerMember
		if err := rows.Scan(&mem.ServerID, &mem.UserID, &mem.Role, &mem.JoinedAt, &mem.Nickname, &mem.Tag); err != nil {
			return nil, err
		}
		out = append(out, mem)
	}
	return out, rows.Err()
}

// ---- channels ----

func (m *MySQL) CreateChannel(ctx context.Context, c *Channel) error {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO channels (server_id, name, type, category_id, position) VALUES (?, ?, ?, ?, ?)`,
		c.ServerID, c.Name, c.Type, c.CategoryID, c.Position)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	c.ID = id
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM channels WHERE id = ?`, id).
		Scan(&c.CreatedAt)
}

const channelCols = `id, server_id, name, type, category_id, position, created_at`

// scanChannel 扫描一行频道（category_id 可空）。
func scanChannel(rows interface{ Scan(...any) error }) (*Channel, error) {
	var c Channel
	var catID sql.NullInt64
	err := rows.Scan(&c.ID, &c.ServerID, &c.Name, &c.Type, &catID, &c.Position, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if catID.Valid {
		c.CategoryID = &catID.Int64
	}
	return &c, nil
}

func (m *MySQL) ChannelByID(ctx context.Context, id int64) (*Channel, error) {
	return scanChannel(m.db.QueryRowContext(ctx,
		`SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
}

func (m *MySQL) ChannelsOfServer(ctx context.Context, serverID int64) ([]Channel, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+channelCols+` FROM channels WHERE server_id = ? ORDER BY position, id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (m *MySQL) UpdateChannel(ctx context.Context, c *Channel) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE channels SET name = ?, type = ?, category_id = ?, position = ? WHERE id = ?`,
		c.Name, c.Type, c.CategoryID, c.Position, c.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (m *MySQL) DeleteChannel(ctx context.Context, id int64) error {
	if _, err := m.db.ExecContext(ctx, `DELETE FROM messages WHERE channel_id = ?`, id); err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// ---- categories（v0.1.2 迁移 0003） ----

func (m *MySQL) CreateCategory(ctx context.Context, c *Category) error {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO categories (server_id, name, position) VALUES (?, ?, ?)`,
		c.ServerID, c.Name, c.Position)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	c.ID = id
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM categories WHERE id = ?`, id).
		Scan(&c.CreatedAt)
}

const categoryCols = `id, server_id, name, position, created_at`

func scanCategory(rows interface{ Scan(...any) error }) (*Category, error) {
	var c Category
	err := rows.Scan(&c.ID, &c.ServerID, &c.Name, &c.Position, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (m *MySQL) CategoryByID(ctx context.Context, id int64) (*Category, error) {
	return scanCategory(m.db.QueryRowContext(ctx,
		`SELECT `+categoryCols+` FROM categories WHERE id = ?`, id))
}

func (m *MySQL) CategoriesOfServer(ctx context.Context, serverID int64) ([]Category, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+categoryCols+` FROM categories WHERE server_id = ? ORDER BY position, id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Category{}
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// DeleteCategoryWithChannels 单事务删除分类及其下全部频道（含各频道消息）。
// 调用方须先确认分类存在（checkAffected 在事务内同样生效）。
func (m *MySQL) DeleteCategoryWithChannels(ctx context.Context, id int64) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id IN (SELECT id FROM channels WHERE category_id = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE category_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM categories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := checkAffected(res); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- messages ----

func (m *MySQL) CreateMessage(ctx context.Context, msg *Message) error {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO messages (channel_id, user_id, content) VALUES (?, ?, ?)`,
		msg.ChannelID, msg.UserID, msg.Content)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	msg.ID = id
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id = ?`, id).
		Scan(&msg.CreatedAt)
}

func (m *MySQL) MessagesBefore(ctx context.Context, channelID, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = m.db.QueryContext(ctx,
			`SELECT m.id, m.channel_id, m.user_id, m.content, m.created_at, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? AND m.id < ? ORDER BY m.id DESC LIMIT ?`, channelID, beforeID, limit)
	} else {
		rows, err = m.db.QueryContext(ctx,
			`SELECT m.id, m.channel_id, m.user_id, m.content, m.created_at, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? ORDER BY m.id DESC LIMIT ?`, channelID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 倒序收集后反转为正序（旧→新）。
	rev := []Message{}
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content,
			&msg.CreatedAt, &msg.SenderNickname, &msg.SenderTag); err != nil {
			return nil, err
		}
		rev = append(rev, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Message, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out, nil
}

// ---- invites ----

func (m *MySQL) CreateInvite(ctx context.Context, inv *Invite) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO invites (code, server_id, creator_id, max_uses, uses, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inv.Code, inv.ServerID, inv.CreatorID, inv.MaxUses, inv.Uses, inv.ExpiresAt)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

const inviteCols = `code, server_id, creator_id, max_uses, uses, expires_at`

func (m *MySQL) InviteByCode(ctx context.Context, code string) (*Invite, error) {
	var inv Invite
	var exp sql.NullTime
	err := m.db.QueryRowContext(ctx, `SELECT `+inviteCols+` FROM invites WHERE code = ?`, code).
		Scan(&inv.Code, &inv.ServerID, &inv.CreatorID, &inv.MaxUses, &inv.Uses, &exp)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exp.Valid {
		t := exp.Time
		inv.ExpiresAt = &t
	}
	return &inv, nil
}

func (m *MySQL) UseInvite(ctx context.Context, code string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE invites SET uses = uses + 1
		 WHERE code = ? AND (max_uses = 0 OR uses < max_uses)`, code)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// 区分不存在与已用尽。
		var cnt int
		if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites WHERE code = ?`, code).Scan(&cnt); err != nil {
			return err
		}
		if cnt == 0 {
			return ErrNotFound
		}
		return ErrConflict
	}
	return nil
}

// ---- 工具 ----

// isDup 判断 MySQL 唯一键冲突（1062）。
func isDup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

func checkAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
