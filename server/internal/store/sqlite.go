package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动（无 cgo），CGO_ENABLED=0 可构建
)

// SQLite 是 Store 的 SQLite 实现（modernc.org/sqlite，完整第三实现，不是换驱动）。
// 与 MySQL 的 DML 方言差异各写各的：isDupSQLite 判 UNIQUE 约束、upsert 用
// ON CONFLICT DO UPDATE、时间比较用 CURRENT_TIMESTAMP、迁移互斥靠 BEGIN IMMEDIATE。
type SQLite struct {
	db *sql.DB
}

// sqliteDSN 统一连接参数：
//   - busy_timeout：锁竞争时等待而非立即 SQLITE_BUSY；
//   - journal_mode(WAL)：读写并发（单写者 + 多读者）；
//   - foreign_keys(1)：与 MySQL 对齐（本 schema 无 FK 声明，开了也不影响删除顺序）；
//   - _time_format=datetime + _timezone=UTC：time.Time 读写都是
//     "2006-01-02 15:04:05" UTC 文本，与 CURRENT_TIMESTAMP 同格式同时区，
//     expires_at > CURRENT_TIMESTAMP 这类比较才不会因字符串格式分叉而失真；
//   - _txlock=immediate：BeginTx 即 BEGIN IMMEDIATE，迁移整轮与事务写互斥。
func sqliteDSN(path string) string {
	return "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_time_format=datetime&_timezone=UTC&_txlock=immediate"
}

// OpenSQLite 打开（必要时创建）path 处的库；path 为空时取 LITETALK_SQLITE_PATH。
// 权限（#88）：数据目录 0700、库文件与 -wal/-shm 0600——库里有密码哈希、会话
// 和邀请数据，同机其他用户不能读。旧部署已有的宽松权限在打开时就地收紧。
// MaxOpenConns(1)：SQLite 单写者，连接池 >1 只会在 busy_timeout 边缘打转
// （常见坑：SQLITE_BUSY 风暴），并发写让单连接排队反而更稳。
func OpenSQLite(path string) (*SQLite, error) {
	if v := os.Getenv("LITETALK_SQLITE_PATH"); v != "" {
		path = v
	}
	if path == "" {
		return nil, errors.New("sqlite: 库路径为空")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." && dir != path {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sqlite: 建数据目录: %w", err)
		}
	}
	// 先以 0600 预创建库文件与 -wal/-shm：SQLite 首次连接建库会按进程 umask
	// 落成 0644，WAL 文件更是要到首次写入才创建，这里把可读窗口和时序问题一起消掉。
	if err := precreateSQLiteFiles(path); err != nil {
		return nil, fmt.Errorf("sqlite: 创建库文件: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	// Ping 会按 DSN 打开 WAL，-wal/-shm 可能在这之后才存在；统一在打开完成后收紧。
	// Windows 无 POSIX 权限位，该函数是 no-op（见 sqlite_perms_windows.go）。
	if err := tightenSQLitePerms(dir, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Close 关闭连接池。
func (s *SQLite) Close() error { return s.db.Close() }

// DB 暴露底层句柄（迁移工具用）。
func (s *SQLite) DB() *sql.DB { return s.db }

// Migrate 按文件名顺序执行 fsys 下的 *.up.sql，已执行过的跳过（记 schema_migrations）。
// 互斥：不用 GET_LOCK，整轮包进单事务——DSN 的 _txlock=immediate 使其为
// BEGIN IMMEDIATE，同库第二个迁移进程拿不到写锁会在 busy_timeout 后报错。
func (s *SQLite) Migrate(ctx context.Context, fsys fs.FS) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// SQLite 版不用 VARCHAR/DATETIME：TEXT 亲和更可控，避免类型亲和意外。
	if _, err := tx.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
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
		if err := tx.QueryRowContext(ctx,
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
		stmts, err := splitSQL(string(body))
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		// 触发器/存储过程会被 splitSQL 明确拒绝（#75），逐条执行即可。
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RollbackLast 执行最近一个已应用版本的 *.down.sql（验证 down 迁移用）。
func (s *SQLite) RollbackLast(ctx context.Context, fsys fs.FS) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version string
	err = tx.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	body, err := fs.ReadFile(fsys, version+".down.sql")
	if err != nil {
		return err
	}
	stmts, err := splitSQL(string(body))
	if err != nil {
		return fmt.Errorf("parse %s.down.sql: %w", version, err)
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rollback %s: %w", version, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = ?`, version); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- users ----

func (s *SQLite) CreateUser(ctx context.Context, u *User) error {
	u.ID = newID(u.ID)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash, email_verified, is_guest, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Nickname, u.Tag, u.PasswordHash, u.EmailVerified, u.IsGuest, u.Status)
	if err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM users WHERE id = ?`, u.ID).
		Scan(&u.CreatedAt, &u.UpdatedAt)
}

func (s *SQLite) scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Nickname, &u.Tag, &u.PasswordHash,
		&u.EmailVerified, &u.IsGuest, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

const sqUserCols = `id, email, nickname, tag, password_hash, email_verified, is_guest, status, created_at, updated_at`

func (s *SQLite) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `SELECT `+sqUserCols+` FROM users WHERE id = ?`, id))
}

func (s *SQLite) UserByEmail(ctx context.Context, email string) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `SELECT `+sqUserCols+` FROM users WHERE email = ?`, email))
}

func (s *SQLite) UserByNicknameTag(ctx context.Context, nickname, tag string) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+sqUserCols+` FROM users WHERE nickname = ? AND tag = ?`, nickname, tag))
}

// UpdateUser 显式写 updated_at（SQLite 无 ON UPDATE CURRENT_TIMESTAMP）。
// 同值 UPDATE 时 SQLite 仍计 1 行受影响（与 MySQL 无 clientFoundRows 不同），
// checkAffected 不会把「原样保存」误报成 ErrNotFound。
func (s *SQLite) UpdateUser(ctx context.Context, u *User) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET nickname = ?, tag = ?, password_hash = ?, email_verified = ?, status = ?,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		u.Nickname, u.Tag, u.PasswordHash, u.EmailVerified, u.Status, u.ID)
	if err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return checkAffected(res)
}

// HasUsers 库内是否已有用户（首启探测；UserByID(1) 随 Snowflake 失效）。
func (s *SQLite) HasUsers(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users LIMIT 1`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// HasServers 库内是否已有服务器（#27：实例是否已被认领的持久判据）。
func (s *SQLite) HasServers(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers LIMIT 1`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- friendships ----

func (s *SQLite) CreateFriendship(ctx context.Context, f *Friendship) error {
	f.ID = newID(f.ID)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO friendships (id, user_id, friend_id, status, remark, group_name)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.ID, f.UserID, f.FriendID, f.Status, f.Remark, f.GroupName)
	if err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at FROM friendships WHERE id = ?`, f.ID).
		Scan(&f.CreatedAt)
}

const sqFriendshipCols = `id, user_id, friend_id, status, remark, group_name, created_at`

func (s *SQLite) FriendshipByID(ctx context.Context, id int64) (*Friendship, error) {
	var f Friendship
	err := s.db.QueryRowContext(ctx,
		`SELECT `+sqFriendshipCols+` FROM friendships WHERE id = ?`, id).
		Scan(&f.ID, &f.UserID, &f.FriendID, &f.Status, &f.Remark, &f.GroupName, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *SQLite) FriendshipsOfUser(ctx context.Context, userID int64) ([]Friendship, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqFriendshipCols+` FROM friendships WHERE user_id = ? OR friend_id = ?`, userID, userID)
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

func (s *SQLite) FriendshipBetween(ctx context.Context, a, b int64) (*Friendship, error) {
	var f Friendship
	err := s.db.QueryRowContext(ctx,
		`SELECT `+sqFriendshipCols+` FROM friendships
		 WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)`, a, b, b, a).
		Scan(&f.ID, &f.UserID, &f.FriendID, &f.Status, &f.Remark, &f.GroupName, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *SQLite) UpdateFriendship(ctx context.Context, f *Friendship) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE friendships SET status = ?, remark = ?, group_name = ? WHERE id = ?`,
		f.Status, f.Remark, f.GroupName, f.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *SQLite) DeleteFriendship(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM friendships WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// ---- servers ----

func (s *SQLite) CreateServer(ctx context.Context, sv *Server) error {
	sv.ID = newID(sv.ID)
	// server + 三条 builtin 角色同事务（#35）：缺任一条后续 AddMember/SetMemberRoles 会断不变量。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO servers (id, name, owner_id, icon_name, icon_color) VALUES (?, ?, ?, ?, ?)`,
		sv.ID, sv.Name, sv.OwnerID, sv.IconName, sv.IconColor); err != nil {
		return err
	}
	for _, tpl := range BuiltinRoleTemplates {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO roles (id, server_id, name, color, position, permissions, builtin_key, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newID(0), sv.ID, tpl.Name, DefaultRoleColor, tpl.Position, int64(tpl.Permissions),
			tpl.BuiltinKey, time.Now(), time.Now()); err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM servers WHERE id = ?`, sv.ID).
		Scan(&sv.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

const sqServerCols = `id, name, owner_id, icon_name, icon_color, created_at`

func (s *SQLite) ServerByID(ctx context.Context, id int64) (*Server, error) {
	var sv Server
	err := s.db.QueryRowContext(ctx, `SELECT `+sqServerCols+` FROM servers WHERE id = ?`, id).
		Scan(&sv.ID, &sv.Name, &sv.OwnerID, &sv.IconName, &sv.IconColor, &sv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sv, nil
}

func (s *SQLite) ServersOfUser(ctx context.Context, userID int64) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.owner_id, s.icon_name, s.icon_color, s.created_at
		 FROM servers s JOIN server_members m ON m.server_id = s.id
		 WHERE m.user_id = ? ORDER BY s.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		var sv Server
		if err := rows.Scan(&sv.ID, &sv.Name, &sv.OwnerID, &sv.IconName, &sv.IconColor, &sv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdateServer(ctx context.Context, sv *Server) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE servers SET name = ?, icon_name = ?, icon_color = ? WHERE id = ?`,
		sv.Name, sv.IconName, sv.IconColor, sv.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// DeleteServer 单事务删除主体 + 级联子资源（#22）：中途失败时整体回滚。
func (s *SQLite) DeleteServer(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id IN (SELECT id FROM channels WHERE server_id = ?)`, id); err != nil {
		return err
	}
	// 覆盖挂在频道上，必须赶在 channels 删除前清（#35）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_overwrites WHERE channel_id IN (SELECT id FROM channels WHERE server_id = ?)`, id); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM member_roles WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM roles WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE server_id = ?`, id); err != nil {
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

// ---- members ----

func (s *SQLite) AddMember(ctx context.Context, mem *ServerMember) error {
	// 成员 + 默认角色同事务（#35）；冲突不写分配。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := dbAddMemberWithDefaults(ctx, tx, mem, isDupSQLite); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) RemoveMember(ctx context.Context, serverID, userID int64) error {
	// 成员行 + 其角色分配 + 该服内以该成员为目标的覆盖一起清（#35）。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM server_members WHERE server_id = ? AND user_id = ?`, serverID, userID)
	if err != nil {
		return err
	}
	if err := checkAffected(res); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM member_roles WHERE server_id = ? AND user_id = ?`, serverID, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_overwrites
		  WHERE target_type = 'member' AND target_id = ?
		    AND channel_id IN (SELECT id FROM channels WHERE server_id = ?)`, userID, serverID); err != nil {
		return err
	}
	return tx.Commit()
}

const sqMemberJoin = `SELECT m.server_id, m.user_id, m.role, m.joined_at, u.nickname, u.tag
	FROM server_members m JOIN users u ON u.id = m.user_id`

func (s *SQLite) Member(ctx context.Context, serverID, userID int64) (*ServerMember, error) {
	var mem ServerMember
	err := s.db.QueryRowContext(ctx, sqMemberJoin+` WHERE m.server_id = ? AND m.user_id = ?`, serverID, userID).
		Scan(&mem.ServerID, &mem.UserID, &mem.Role, &mem.JoinedAt, &mem.Nickname, &mem.Tag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &mem, nil
}

func (s *SQLite) MembersOfServer(ctx context.Context, serverID int64) ([]ServerMember, error) {
	rows, err := s.db.QueryContext(ctx, sqMemberJoin+` WHERE m.server_id = ? ORDER BY m.joined_at`, serverID)
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

func (s *SQLite) CreateChannel(ctx context.Context, c *Channel) error {
	c.ID = newID(c.ID)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, category_id, position) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.ServerID, c.Name, c.Type, c.CategoryID, c.Position)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at FROM channels WHERE id = ?`, c.ID).
		Scan(&c.CreatedAt)
}

const sqChannelCols = `id, server_id, name, type, category_id, position, created_at`

// scanChannel 扫描一行频道（category_id 可空）。
func scanChannelLite(rows interface{ Scan(...any) error }) (*Channel, error) {
	var c Channel
	var catID sql.NullInt64
	err := rows.Scan(&c.ID, &c.ServerID, &c.Name, &c.Type, &catID, &c.Position, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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

func (s *SQLite) ChannelByID(ctx context.Context, id int64) (*Channel, error) {
	return scanChannelLite(s.db.QueryRowContext(ctx,
		`SELECT `+sqChannelCols+` FROM channels WHERE id = ?`, id))
}

func (s *SQLite) ChannelsOfServer(ctx context.Context, serverID int64) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqChannelCols+` FROM channels WHERE server_id = ? ORDER BY position, id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		c, err := scanChannelLite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdateChannel(ctx context.Context, c *Channel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET name = ?, type = ?, category_id = ?, position = ? WHERE id = ?`,
		c.Name, c.Type, c.CategoryID, c.Position, c.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// DeleteChannel 单事务：消息 + 频道覆盖 + 频道一起删（#22/#35）。
func (s *SQLite) DeleteChannel(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE channel_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_overwrites WHERE channel_id = ?`, id); err != nil {
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

func (s *SQLite) CreateCategory(ctx context.Context, c *Category) error {
	c.ID = newID(c.ID)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO categories (id, server_id, name, position) VALUES (?, ?, ?, ?)`,
		c.ID, c.ServerID, c.Name, c.Position)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at FROM categories WHERE id = ?`, c.ID).
		Scan(&c.CreatedAt)
}

const sqCategoryCols = `id, server_id, name, position, created_at`

func scanCategoryLite(rows interface{ Scan(...any) error }) (*Category, error) {
	var c Category
	err := rows.Scan(&c.ID, &c.ServerID, &c.Name, &c.Position, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *SQLite) CategoryByID(ctx context.Context, id int64) (*Category, error) {
	return scanCategoryLite(s.db.QueryRowContext(ctx,
		`SELECT `+sqCategoryCols+` FROM categories WHERE id = ?`, id))
}

func (s *SQLite) CategoriesOfServer(ctx context.Context, serverID int64) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqCategoryCols+` FROM categories WHERE server_id = ? ORDER BY position, id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Category{}
	for rows.Next() {
		c, err := scanCategoryLite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// DeleteCategoryWithChannels 单事务删除分类及其下全部频道（含各频道消息与覆盖）。
func (s *SQLite) DeleteCategoryWithChannels(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id IN (SELECT id FROM channels WHERE category_id = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_overwrites WHERE channel_id IN (SELECT id FROM channels WHERE category_id = ?)`, id); err != nil {
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

// UpdateCategory 全量写 name/position。SQLite 同值 UPDATE 也计 1 行受影响，
// 但保留「0 行且行存在 → 成功」兜底与 MySQL/Mem 对齐（原样保存不许误报 404）。
func (s *SQLite) UpdateCategory(ctx context.Context, c *Category) error {
	res, err := s.db.ExecContext(ctx,
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
	if _, err := s.CategoryByID(ctx, c.ID); err != nil {
		return err
	}
	return nil
}

// ReorderCategories 把 id 移到 position（0-based，越界夹到首/尾），同服务器
// 其它分类相对顺序保持并重排为 0..n-1（单事务）。
func (s *SQLite) ReorderCategories(ctx context.Context, serverID, id int64, position int) error {
	tx, err := s.db.BeginTx(ctx, nil)
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

func (s *SQLite) CreateMessage(ctx context.Context, msg *Message) error {
	msg.ID = newID(msg.ID)
	// 空 nonce 落 NULL：唯一索引 (channel_id, user_id, nonce) 上 '' 会互相撞，NULL 不受约束。
	var nonce any
	if msg.Nonce != "" {
		nonce = msg.Nonce
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content, nonce) VALUES (?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.UserID, msg.Content, nonce)
	if err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id = ?`, msg.ID).
		Scan(&msg.CreatedAt)
}

// sqMessageCols 列清单（nonce 经 COALESCE 归一空串）。
const sqMessageCols = `m.id, m.channel_id, m.user_id, m.content, COALESCE(m.nonce, ''), m.created_at`

func (s *SQLite) MessagesBefore(ctx context.Context, channelID, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+sqMessageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? AND m.id < ? ORDER BY m.id DESC LIMIT ?`, channelID, beforeID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+sqMessageCols+`, u.nickname, u.tag
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
func (s *SQLite) MessagesAfter(ctx context.Context, channelID, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if afterID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+sqMessageCols+`, u.nickname, u.tag
			 FROM messages m JOIN users u ON u.id = m.user_id
			 WHERE m.channel_id = ? AND m.id > ? ORDER BY m.id ASC LIMIT ?`, channelID, afterID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+sqMessageCols+`, u.nickname, u.tag
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

// MessageByNonce 幂等回读原消息（带发送者内联信息，响应直接可用；频道维度隔离）。
func (s *SQLite) MessageByNonce(ctx context.Context, channelID, userID int64, nonce string) (*Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sqMessageCols+`, u.nickname, u.tag
		 FROM messages m JOIN users u ON u.id = m.user_id
		 WHERE m.channel_id = ? AND m.user_id = ? AND m.nonce = ?`, channelID, userID, nonce)
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

func (s *SQLite) CreateInvite(ctx context.Context, inv *Invite) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (code, server_id, creator_id, max_uses, uses, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inv.Code, inv.ServerID, inv.CreatorID, inv.MaxUses, inv.Uses, inv.ExpiresAt)
	if err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

const sqInviteCols = `code, server_id, creator_id, max_uses, uses, expires_at`

func (s *SQLite) InviteByCode(ctx context.Context, code string) (*Invite, error) {
	var inv Invite
	var exp sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT `+sqInviteCols+` FROM invites WHERE code = ?`, code).
		Scan(&inv.Code, &inv.ServerID, &inv.CreatorID, &inv.MaxUses, &inv.Uses, &exp)
	if errors.Is(err, sql.ErrNoRows) {
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

// UseInvite 原子路径含 max_uses 与过期校验（#22）：SQLite 方言用 CURRENT_TIMESTAMP
// （与 expires_at 同为 UTC "YYYY-MM-DD HH:MM:SS" 文本，字典序即时间序）。
func (s *SQLite) UseInvite(ctx context.Context, code string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := sqUseInvite(ctx, tx, code)
	if err != nil {
		return err
	}
	if !ok {
		return sqInviteMiss(ctx, tx, code)
	}
	return tx.Commit()
}

// sqUseInvite 邀请码自增（单条 UPDATE 即原子判定 max_uses/过期）；返回是否命中。
// 不 Commit——调用方决定事务边界（UseInvite 与 UseInviteAndAddMember 共用）。
func sqUseInvite(ctx context.Context, tx *sql.Tx, code string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE invites SET uses = uses + 1
		 WHERE code = ? AND (max_uses = 0 OR uses < max_uses)
		   AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)`, code)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// sqInviteMiss 区分 UPDATE 0 行的原因：不存在、已过期、次数用尽。
func sqInviteMiss(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, code string) error {
	var cnt, expired int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(expires_at IS NOT NULL AND expires_at <= CURRENT_TIMESTAMP), 0)
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

// UseInviteAndAddMember 同一事务内消费邀请码并插入成员（#22）：成员冲突或
// 插入失败时整体回滚，邀请码次数不增长、默认角色分配不落库（#35）。
func (s *SQLite) UseInviteAndAddMember(ctx context.Context, code string, userID int64, role MemberRole) (*ServerMember, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var serverID int64
	err = tx.QueryRowContext(ctx, `SELECT server_id FROM invites WHERE code = ?`, code).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ok, err := sqUseInvite(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, sqInviteMiss(ctx, tx, code)
	}
	mem := &ServerMember{ServerID: serverID, UserID: userID, Role: role, JoinedAt: time.Now()}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_members (server_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`,
		mem.ServerID, mem.UserID, mem.Role, mem.JoinedAt); err != nil {
		if isDupSQLite(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	if err := dbAssignDefaultRoles(ctx, tx, mem.ServerID, mem.UserID, mem.Role, 0, mem.JoinedAt, isDupSQLite); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return mem, nil
}

// ---- user_sessions（v1.1.0 迁移 0004） ----

// UpsertUserSession 会话覆盖写。SQLite 不认 VALUES() 函数，必须用 excluded.*；
// updated_at 无 ON UPDATE CURRENT_TIMESTAMP，这里显式写 CURRENT_TIMESTAMP。
func (s *SQLite) UpsertUserSession(ctx context.Context, sess *UserSession) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_sessions (user_id, device_type, sid, ip)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, device_type) DO UPDATE SET
		   sid = excluded.sid, ip = excluded.ip, updated_at = CURRENT_TIMESTAMP`,
		sess.UserID, sess.DeviceType, sess.Sid, sess.IP)
	return err
}

func (s *SQLite) GetUserSession(ctx context.Context, userID int64, deviceType string) (*UserSession, error) {
	sess := &UserSession{}
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, device_type, sid, ip, created_at, updated_at
		 FROM user_sessions WHERE user_id = ? AND device_type = ?`,
		userID, deviceType).Scan(&sess.UserID, &sess.DeviceType, &sess.Sid, &sess.IP, &sess.CreatedAt, &sess.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *SQLite) DeleteUserSession(ctx context.Context, userID int64, deviceType string) error {
	res, err := s.db.ExecContext(ctx,
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

func (s *SQLite) DeleteAllUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ?`, userID)
	return err
}

// ---- roles / member_roles / channel_overwrites / audit_log（迁移 0010，#35 B1） ----
// 语义实现与 MySQL 共用 roles.go 的 db* 函数，这里只做方言接线。

func (s *SQLite) ListRoles(ctx context.Context, serverID int64) ([]Role, error) {
	return dbListRoles(ctx, s.db, serverID)
}

func (s *SQLite) RoleByID(ctx context.Context, id int64) (*Role, error) {
	return dbRoleByID(ctx, s.db, id)
}

func (s *SQLite) CreateRole(ctx context.Context, r *Role) error {
	return dbCreateRole(ctx, s.db, r, isDupSQLite)
}

func (s *SQLite) UpdateRole(ctx context.Context, r *Role) error {
	return dbUpdateRole(ctx, s.db, r)
}

func (s *SQLite) DeleteRole(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := dbDeleteRole(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) RolesOfMember(ctx context.Context, serverID, userID int64) ([]Role, error) {
	return dbRolesOfMember(ctx, s.db, serverID, userID)
}

func (s *SQLite) SetMemberRoles(ctx context.Context, serverID, userID int64, roleIDs []int64, assignedBy int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := dbSetMemberRoles(ctx, tx, serverID, userID, roleIDs, assignedBy); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) RoleAssignmentsOfServer(ctx context.Context, serverID int64) ([]MemberRoleAssignment, error) {
	return dbRoleAssignmentsOfServer(ctx, s.db, serverID)
}

func (s *SQLite) ChannelOverwrites(ctx context.Context, channelID int64) ([]ChannelOverwrite, error) {
	return dbChannelOverwrites(ctx, s.db, channelID)
}

func (s *SQLite) ChannelOverwritesOfServer(ctx context.Context, serverID int64) ([]ChannelOverwrite, error) {
	return dbChannelOverwritesOfServer(ctx, s.db, serverID)
}

// SetChannelOverwrite upsert。SQLite 不认 VALUES()，必须用 excluded.*；
// updated_at 无 ON UPDATE CURRENT_TIMESTAMP，显式写。
func (s *SQLite) SetChannelOverwrite(ctx context.Context, o *ChannelOverwrite) error {
	if err := dbOverwriteTargetOK(ctx, s.db, o); err != nil {
		return err
	}
	o.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_overwrites (channel_id, target_type, target_id, allow, deny, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(channel_id, target_type, target_id) DO UPDATE SET
		   allow = excluded.allow, deny = excluded.deny, updated_at = excluded.updated_at`,
		o.ChannelID, o.TargetType, o.TargetID, int64(o.Allow), int64(o.Deny), o.UpdatedAt)
	return err
}

func (s *SQLite) DeleteChannelOverwrite(ctx context.Context, channelID int64, targetType OverwriteTargetType, targetID int64) error {
	return dbDeleteChannelOverwrite(ctx, s.db, channelID, targetType, targetID)
}

func (s *SQLite) WriteAudit(ctx context.Context, a *AuditLog) error {
	return dbWriteAudit(ctx, s.db, a, isDupSQLite)
}

func (s *SQLite) AuditLogBefore(ctx context.Context, serverID, beforeID int64, limit int) ([]AuditLog, error) {
	return dbAuditLogBefore(ctx, s.db, serverID, beforeID, limit)
}

// ---- 工具 ----

// isDupSQLite 判断 SQLite 唯一约束冲突（与 mysql.go 的 isDup 各写各的）。
// modernc 文案形如：constraint failed: UNIQUE constraint failed: users.email (2067)。
func isDupSQLite(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// 编译期断言：SQLite 是 Store 的完整实现，无降级方法。
var _ Store = (*SQLite)(nil)
