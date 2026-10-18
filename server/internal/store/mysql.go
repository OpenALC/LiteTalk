package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// Migrate 按文件名顺序执行 fsys 下的 *.up.sql（golang-migrate 命名约定），
// 已执行过的跳过（记录在 schema_migrations）。
// 启动互斥：GET_LOCK('litetalk_migrate', 60) 防多实例并发迁移；拿不到锁返回错误。
func (m *MySQL) Migrate(ctx context.Context, fsys fs.FS) error {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// 同一连接拿锁/解锁——连接池里两条不同连接会锁不住。
	var got int
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('litetalk_migrate', 60)`).Scan(&got); err != nil {
		return fmt.Errorf("migrate lock: %w", err)
	}
	if got != 1 {
		return errors.New("migrate lock: 60s 内另一实例正在迁移")
	}
	defer conn.ExecContext(ctx, `SELECT RELEASE_LOCK('litetalk_migrate')`)

	if _, err := conn.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(255) PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(fsys, ".")
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
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		// MySQL 驱动不支持多语句 Exec，逐条执行。
		for _, stmt := range splitSQL(string(body)) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return err
		}
	}
	return nil
}

// RollbackLast 执行最近一个已应用版本的 *.down.sql（验证 down 迁移用）。
func (m *MySQL) RollbackLast(ctx context.Context, fsys fs.FS) error {
	var version string
	err := m.db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	body, err := fs.ReadFile(fsys, version+".down.sql")
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

// splitSQL 按分号切分简单 SQL 脚本（迁移文件不含存储过程/触发器，BEGIN…END
// 内的分号仍会切碎）。扫描时同步识别注释与引号：注释里的半角分号不再把语句
// 切碎（#75），先剥注释再切分的语义在一趟扫描里完成。
//
// 字符串字面量与引号标识符整体原样拷贝，其中的 ;、--、# 一律不动；引号转义
// 只认成对引号转义（连续两个单引号 / 双引号 / 反引号），不认反斜杠（MySQL
// 专有，迁移里不应出现）。
// -- 与 # 按行注释剥到行尾（MySQL 的 1--2 紧贴算术写法会被吃掉，不要写）；
// 普通块注释剥掉并补一个空格防 token 粘连；MySQL 的 /*! 与 /*+ 有真语义，
// 整体保留（其中的分号同样不切分）。
func splitSQL(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if stmt := strings.TrimSpace(b.String()); stmt != "" {
			out = append(out, stmt)
		}
		b.Reset()
	}
	for i := 0; i < len(s); {
		c := s[i]
		// 引号区域（字符串字面量 / 引号标识符）：原样拷贝，只认成对引号转义。
		if c == '\'' || c == '"' || c == '`' {
			b.WriteByte(c)
			i++
			for i < len(s) {
				b.WriteByte(s[i])
				if s[i] != c {
					i++
					continue
				}
				if i+1 < len(s) && s[i+1] == c { // '' 形式转义，仍在引号内
					b.WriteByte(c)
					i += 2
					continue
				}
				i++
				break
			}
			continue
		}
		// 行注释剥到行尾；换行留下，不改变后续语句的行结构。
		if c == '#' || (c == '-' && i+1 < len(s) && s[i+1] == '-') {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		// 块注释：/*! 与 /*+ 原样保留，其余剥掉补空格防相邻 token 粘连。
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			keep := i+2 < len(s) && (s[i+2] == '!' || s[i+2] == '+')
			if keep {
				b.WriteString(s[i : i+3])
				i += 3
			} else {
				b.WriteByte(' ')
				i += 2
			}
			for i < len(s) {
				if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
					if keep {
						b.WriteString("*/")
					}
					i += 2
					break
				}
				if keep {
					b.WriteByte(s[i])
				}
				i++
			}
			continue
		}
		if c == ';' {
			flush()
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	flush()
	return out
}

// ---- users ----

func (m *MySQL) CreateUser(ctx context.Context, u *User) error {
	u.ID = newID(u.ID)
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash, email_verified, is_guest, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Nickname, u.Tag, u.PasswordHash, u.EmailVerified, u.IsGuest, u.Status)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return m.db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM users WHERE id = ?`, u.ID).
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

// HasUsers 库内是否已有用户（首启探测；UserByID(1) 随 Snowflake 失效）。
func (m *MySQL) HasUsers(ctx context.Context) (bool, error) {
	var n int
	err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users LIMIT 1`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- friendships ----

func (m *MySQL) CreateFriendship(ctx context.Context, f *Friendship) error {
	f.ID = newID(f.ID)
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO friendships (id, user_id, friend_id, status) VALUES (?, ?, ?, ?)`,
		f.ID, f.UserID, f.FriendID, f.Status)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM friendships WHERE id = ?`, f.ID).
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
	s.ID = newID(s.ID)
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO servers (id, name, owner_id, icon_name, icon_color) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.OwnerID, s.IconName, s.IconColor)
	if err != nil {
		return err
	}
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM servers WHERE id = ?`, s.ID).
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
	// 单事务删除主体 + 级联子资源（#22）：中途失败时整体回滚。
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id IN (SELECT id FROM channels WHERE server_id = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM categories WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_members WHERE server_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := checkAffected(res); err != nil {
		return err
	}
	return tx.Commit()
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
	c.ID = newID(c.ID)
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, category_id, position) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.ServerID, c.Name, c.Type, c.CategoryID, c.Position)
	if err != nil {
		return err
	}
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM channels WHERE id = ?`, c.ID).
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
	// 单事务：消息 + 频道一起删（#22）。
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE channel_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := checkAffected(res); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- categories（v0.1.2 迁移 0003） ----

func (m *MySQL) CreateCategory(ctx context.Context, c *Category) error {
	c.ID = newID(c.ID)
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO categories (id, server_id, name, position) VALUES (?, ?, ?, ?)`,
		c.ID, c.ServerID, c.Name, c.Position)
	if err != nil {
		return err
	}
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM categories WHERE id = ?`, c.ID).
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

func (m *MySQL) UpdateCategory(ctx context.Context, c *Category) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE categories SET name = ?, position = ? WHERE id = ?`,
		c.Name, c.Position, c.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// MySQL 未开 clientFoundRows：值全同现值时同样报 0 行。行存在即成功（与
	// Mem 对齐，否则「原样保存」会被误报 404），行不存在才 ErrNotFound。
	if _, err := m.CategoryByID(ctx, c.ID); err != nil {
		return err
	}
	return nil
}

// ReorderCategories 把 id 移到 position（0-based，越界夹到首/尾），同服务器
// 其它分类相对顺序保持并重排为 0..n-1（单事务）。
func (m *MySQL) ReorderCategories(ctx context.Context, serverID, id int64, position int) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Tx 同时只允许一个活跃查询：先把 id 集读完并关闭 rows，再逐条 UPDATE。
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM categories WHERE server_id = ? ORDER BY position, id`, serverID)
	if err != nil {
		return err
	}
	ids := []int64{}
	found := false
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return err
		}
		if cid == id {
			found = true
		}
		ids = append(ids, cid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	rest := make([]int64, 0, len(ids))
	for _, cid := range ids {
		if cid != id {
			rest = append(rest, cid)
		}
	}
	if position < 0 {
		position = 0
	}
	if position > len(rest) {
		position = len(rest)
	}
	out := make([]int64, 0, len(ids))
	out = append(out, rest[:position]...)
	out = append(out, id)
	out = append(out, rest[position:]...)
	for i, cid := range out {
		if _, err := tx.ExecContext(ctx,
			`UPDATE categories SET position = ? WHERE id = ?`, i, cid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- messages ----

func (m *MySQL) CreateMessage(ctx context.Context, msg *Message) error {
	msg.ID = newID(msg.ID)
	// 空 nonce 落 NULL：唯一索引 (user_id, nonce) 上 '' 会互相撞，NULL 不受约束。
	var nonce any
	if msg.Nonce != "" {
		nonce = msg.Nonce
	}
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content, nonce) VALUES (?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.UserID, msg.Content, nonce)
	if err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return m.db.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id = ?`, msg.ID).
		Scan(&msg.CreatedAt)
}

// messageCols 列清单（nonce 经 COALESCE 归一空串，免 sql.NullString）。
const messageCols = `m.id, m.channel_id, m.user_id, m.content, COALESCE(m.nonce, ''), m.created_at`

func (m *MySQL) MessagesBefore(ctx context.Context, channelID, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = m.db.QueryContext(ctx,
			`SELECT `+messageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? AND m.id < ? ORDER BY m.id DESC LIMIT ?`, channelID, beforeID, limit)
	} else {
		rows, err = m.db.QueryContext(ctx,
			`SELECT `+messageCols+`, u.nickname, u.tag
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
		if err := rows.Scan(&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content, &msg.Nonce, &msg.CreatedAt,
			&msg.SenderNickname, &msg.SenderTag); err != nil {
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

// MessagesAfter 正序分页（向前翻）：id > afterID 的最早 limit 条，按 id 正序返回。
func (m *MySQL) MessagesAfter(ctx context.Context, channelID, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if afterID > 0 {
		rows, err = m.db.QueryContext(ctx,
			`SELECT `+messageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? AND m.id > ? ORDER BY m.id ASC LIMIT ?`, channelID, afterID, limit)
	} else {
		rows, err = m.db.QueryContext(ctx,
			`SELECT `+messageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? ORDER BY m.id ASC LIMIT ?`, channelID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content, &msg.Nonce, &msg.CreatedAt,
			&msg.SenderNickname, &msg.SenderTag); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// MessageByNonce 幂等回读原消息（带发送者内联信息，响应直接可用）。
func (m *MySQL) MessageByNonce(ctx context.Context, userID int64, nonce string) (*Message, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT `+messageCols+`, u.nickname, u.tag
		 FROM messages m JOIN users u ON u.id = m.user_id
		 WHERE m.user_id = ? AND m.nonce = ?`, userID, nonce)
	var msg Message
	err := row.Scan(&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content, &msg.Nonce, &msg.CreatedAt,
		&msg.SenderNickname, &msg.SenderTag)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &msg, nil
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
	// 原子路径含 max_uses 与过期校验（#22）：防止「校验通过后被并发放大」。
	res, err := m.db.ExecContext(ctx,
		`UPDATE invites SET uses = uses + 1
		 WHERE code = ? AND (max_uses = 0 OR uses < max_uses)
		   AND (expires_at IS NULL OR expires_at > NOW())`, code)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// 区分不存在、已用尽与已过期。
		var cnt, expired int
		if err := m.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(expires_at IS NOT NULL AND expires_at <= NOW()), 0)
			 FROM invites WHERE code = ?`, code).Scan(&cnt, &expired); err != nil {
			return err
		}
		if cnt == 0 {
			return ErrNotFound
		}
		if expired > 0 {
			return ErrExpired
		}
		return ErrConflict
	}
	return nil
}

// ---- user_sessions（v1.1.0 迁移 0004） ----

func (m *MySQL) UpsertUserSession(ctx context.Context, s *UserSession) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO user_sessions (user_id, device_type, sid, ip)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE sid = VALUES(sid), ip = VALUES(ip), updated_at = CURRENT_TIMESTAMP`,
		s.UserID, s.DeviceType, s.Sid, s.IP)
	return err
}

func (m *MySQL) GetUserSession(ctx context.Context, userID int64, deviceType string) (*UserSession, error) {
	s := &UserSession{}
	err := m.db.QueryRowContext(ctx,
		`SELECT user_id, device_type, sid, ip, created_at, updated_at
		 FROM user_sessions WHERE user_id = ? AND device_type = ?`,
		userID, deviceType).Scan(&s.UserID, &s.DeviceType, &s.Sid, &s.IP, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (m *MySQL) DeleteUserSession(ctx context.Context, userID int64, deviceType string) error {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM user_sessions WHERE user_id = ? AND device_type = ?`, userID, deviceType)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *MySQL) DeleteAllUserSessions(ctx context.Context, userID int64) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ?`, userID)
	return err
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
