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
	// 提及没有外键，必须先于 messages 删除，否则留下孤儿行（#39）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_mentions WHERE message_id IN (
		   SELECT id FROM messages WHERE channel_id IN
		     (SELECT id FROM channels WHERE server_id = ?))`, id); err != nil {
		return err
	}
	// 全文索引项先于 messages 删除（#42）：外部内容表还能读到正文，直接删能清掉词项。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages_fts WHERE rowid IN (
		   SELECT id FROM messages WHERE channel_id IN
		     (SELECT id FROM channels WHERE server_id = ?))`, id); err != nil {
		return err
	}
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
	if err := normalizeServerChannel(c); err != nil {
		return err
	}
	c.ID = newID(c.ID)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, category_id, position, password_hash, user_limit, parent_id, audio_bitrate_kbps, audio_frame_ms, scope, dm_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		c.ID, c.ServerID, c.Name, c.Type, c.CategoryID, c.Position,
		c.PasswordHash, c.UserLimit, c.ParentID, c.AudioBitrateKbps, c.AudioFrameMS, c.Scope)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at FROM channels WHERE id = ?`, c.ID).
		Scan(&c.CreatedAt)
}

const sqChannelCols = `id, server_id, name, type, category_id, position, created_at, password_hash, user_limit, parent_id, audio_bitrate_kbps, audio_frame_ms, scope, dm_key`

// scanChannel 扫描一行频道（server_id/category_id/parent_id/dm_key 可空）。
func scanChannelLite(rows interface{ Scan(...any) error }) (*Channel, error) {
	var c Channel
	var serverID, catID, parentID sql.NullInt64
	var dmKey sql.NullString
	err := rows.Scan(&c.ID, &serverID, &c.Name, &c.Type, &catID, &c.Position, &c.CreatedAt,
		&c.PasswordHash, &c.UserLimit, &parentID, &c.AudioBitrateKbps, &c.AudioFrameMS, &c.Scope, &dmKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if serverID.Valid {
		c.ServerID = serverID.Int64
	}
	if catID.Valid {
		c.CategoryID = &catID.Int64
	}
	if parentID.Valid {
		c.ParentID = &parentID.Int64
	}
	if dmKey.Valid {
		c.DMKey = dmKey.String
	}
	return &c, nil
}

func (s *SQLite) ChannelByID(ctx context.Context, id int64) (*Channel, error) {
	return scanChannelLite(s.db.QueryRowContext(ctx,
		`SELECT `+sqChannelCols+` FROM channels WHERE id = ?`, id))
}

func (s *SQLite) ChannelsOfServer(ctx context.Context, serverID int64) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqChannelCols+` FROM channels WHERE server_id = ? AND scope = 'server' ORDER BY position, id`, serverID)
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

// UpdateChannel 全行写（#37 B1 起含 password_hash/user_limit/parent_id；#38 起含
// server_id/scope/dm_key；#46 起含音质两列）：调用方必须先读再写，清除密码写空串、
// 提升顶层写 NULL；会话行 ServerID=0 写 NULL、DMKey 空串写 NULL，server 行强制清空 dm_key。
func (s *SQLite) UpdateChannel(ctx context.Context, c *Channel) error {
	scope := normalizeChannelScope(c.Scope)
	dmKey := c.DMKey
	if scope == ChannelScopeServer {
		dmKey = ""
	} else if !isConversationScope(scope) {
		return ErrForbidden
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET server_id = ?, name = ?, type = ?, category_id = ?, position = ?,
		 password_hash = ?, user_limit = ?, parent_id = ?, audio_bitrate_kbps = ?, audio_frame_ms = ?,
		 scope = ?, dm_key = ? WHERE id = ?`,
		nullID(c.ServerID), c.Name, c.Type, c.CategoryID, c.Position,
		c.PasswordHash, c.UserLimit, c.ParentID, c.AudioBitrateKbps, c.AudioFrameMS,
		scope, nullString(dmKey), c.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// DeleteChannel 单事务：消息 + 频道覆盖 + 会话成员 + 子频道行 + 频道一起删（#22/#35/#37/#38）。
// 子频道与父同行级联（消息/覆盖/成员一并）；IN 子查询指向 channels，与 MySQL 同形。
func (s *SQLite) DeleteChannel(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 提及没有外键，必须先于 messages 删除（含子频道消息，#39）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_mentions WHERE message_id IN (
		   SELECT id FROM messages WHERE channel_id = ?
		      OR channel_id IN (SELECT id FROM channels WHERE parent_id = ?))`, id, id); err != nil {
		return err
	}
	// 全文索引项先于 messages 删除（#42，含子频道）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages_fts WHERE rowid IN (
		   SELECT id FROM messages WHERE channel_id = ?
		      OR channel_id IN (SELECT id FROM channels WHERE parent_id = ?))`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id = ?
		   OR channel_id IN (SELECT id FROM channels WHERE parent_id = ?)`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_overwrites WHERE channel_id = ?
		   OR channel_id IN (SELECT id FROM channels WHERE parent_id = ?)`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_members WHERE channel_id = ?
		   OR channel_id IN (SELECT id FROM channels WHERE parent_id = ?)`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE parent_id = ?`, id); err != nil {
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

// ---- conversations（迁移 0012，#38 B1） ----

func (s *SQLite) CreateConversation(ctx context.Context, c *Channel, memberIDs []int64) error {
	ids, err := normalizeConversation(c, memberIDs)
	if err != nil {
		return err
	}
	c.ID = newID(c.ID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, category_id, position, password_hash, user_limit, parent_id, scope, dm_key)
		 VALUES (?, NULL, ?, ?, NULL, ?, '', ?, NULL, ?, ?)`,
		c.ID, c.Name, c.Type, c.Position, c.UserLimit, c.Scope, nullString(c.DMKey)); err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	now := time.Now()
	for i, uid := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_members (channel_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`,
			c.ID, uid, conversationMemberRole(c.Scope, i), now); err != nil {
			if isDupSQLite(err) {
				return ErrConflict
			}
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM channels WHERE id = ?`, c.ID).
		Scan(&c.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) FindDirectChannel(ctx context.Context, a, b int64) (*Channel, error) {
	return scanChannelLite(s.db.QueryRowContext(ctx,
		`SELECT `+sqChannelCols+` FROM channels WHERE scope = 'direct' AND dm_key = ?`, directKey(a, b)))
}

func (s *SQLite) ListConversations(ctx context.Context, userID int64) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqChannelCols+` FROM channels c
		   JOIN channel_members cm ON cm.channel_id = c.id
		  WHERE cm.user_id = ? AND c.scope IN ('direct','group')
		  ORDER BY c.created_at DESC, c.id DESC`, userID)
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

// sqChannelMemberJoin 与 MySQL 同形；users 用 LEFT JOIN：成员行存在但用户已删时
// 仍返回成员（昵称留空），与 Mem 实现一致。
const sqChannelMemberJoin = `SELECT cm.channel_id, cm.user_id, cm.role, cm.joined_at, u.nickname, u.tag
	FROM channel_members cm LEFT JOIN users u ON u.id = cm.user_id`

func scanChannelMemberLite(rows interface{ Scan(...any) error }) (*ChannelMember, error) {
	var cm ChannelMember
	var nickname, tag sql.NullString
	err := rows.Scan(&cm.ChannelID, &cm.UserID, &cm.Role, &cm.JoinedAt, &nickname, &tag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if nickname.Valid {
		cm.Nickname = nickname.String
	}
	if tag.Valid {
		cm.Tag = tag.String
	}
	return &cm, nil
}

func (s *SQLite) ChannelMembers(ctx context.Context, channelID int64) ([]ChannelMember, error) {
	rows, err := s.db.QueryContext(ctx,
		sqChannelMemberJoin+` WHERE cm.channel_id = ? ORDER BY cm.joined_at, cm.user_id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelMember{}
	for rows.Next() {
		cm, err := scanChannelMemberLite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cm)
	}
	return out, rows.Err()
}

func (s *SQLite) ChannelMember(ctx context.Context, channelID, userID int64) (*ChannelMember, error) {
	return scanChannelMemberLite(s.db.QueryRowContext(ctx,
		sqChannelMemberJoin+` WHERE cm.channel_id = ? AND cm.user_id = ?`, channelID, userID))
}

// ChannelMembersOfChannels 批量成员（#38 B2）：一条 IN 查询，组内排序与
// ChannelMembers 一致；空入参直接返回空 map，不发 SQL。
func (s *SQLite) ChannelMembersOfChannels(ctx context.Context, channelIDs []int64) (map[int64][]ChannelMember, error) {
	out := map[int64][]ChannelMember{}
	if len(channelIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(channelIDs))
	for i, id := range channelIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		sqChannelMemberJoin+` WHERE cm.channel_id IN (`+placeholders(len(args))+`)
		 ORDER BY cm.channel_id, cm.joined_at, cm.user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		cm, err := scanChannelMemberLite(rows)
		if err != nil {
			return nil, err
		}
		out[cm.ChannelID] = append(out[cm.ChannelID], *cm)
	}
	return out, rows.Err()
}

func (s *SQLite) SetChannelMemberRole(ctx context.Context, channelID, userID int64, role ChannelMemberRole) error {
	role, err := normalizeChannelMemberRole(role)
	if err != nil {
		return err
	}
	var scope ChannelScope
	if err := s.db.QueryRowContext(ctx, `SELECT scope FROM channels WHERE id = ?`, channelID).Scan(&scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if scope != ChannelScopeGroup {
		return ErrForbidden
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_members SET role = ? WHERE channel_id = ? AND user_id = ?`, role, channelID, userID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *SQLite) AddChannelMember(ctx context.Context, channelID, userID int64, role ChannelMemberRole) error {
	role, err := normalizeChannelMemberRole(role)
	if err != nil {
		return err
	}
	var scope ChannelScope
	if err := s.db.QueryRowContext(ctx, `SELECT scope FROM channels WHERE id = ?`, channelID).Scan(&scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if scope != ChannelScopeGroup {
		return ErrForbidden
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO channel_members (channel_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`,
		channelID, userID, role, time.Now())
	if isDupSQLite(err) {
		return ErrConflict
	}
	return err
}

func (s *SQLite) RemoveChannelMember(ctx context.Context, channelID, userID int64) error {
	var scope ChannelScope
	if err := s.db.QueryRowContext(ctx, `SELECT scope FROM channels WHERE id = ?`, channelID).Scan(&scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if scope != ChannelScopeGroup {
		return ErrForbidden
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?`, channelID, userID)
	if err != nil {
		return err
	}
	return checkAffected(res)
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
// DeleteCategoryWithChannels 单事务删除分类及其下全部频道（含各频道消息与覆盖）。
// 子频道同分类（API 保证），删除集按 parent_id 再兜一圈防数据漂移（#37）。
func (s *SQLite) DeleteCategoryWithChannels(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 提及没有外键，必须先于 messages 删除（#39）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_mentions WHERE message_id IN (
		   SELECT id FROM messages WHERE channel_id IN (
		     SELECT id FROM channels WHERE category_id = ?
		     UNION
		     SELECT id FROM channels WHERE parent_id IN (SELECT id FROM channels WHERE category_id = ?)
		   ))`, id, id); err != nil {
		return err
	}
	// 全文索引项先于 messages 删除（#42，分类下全部频道与子频道）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages_fts WHERE rowid IN (
		   SELECT id FROM messages WHERE channel_id IN (
		     SELECT id FROM channels WHERE category_id = ?
		     UNION
		     SELECT id FROM channels WHERE parent_id IN (SELECT id FROM channels WHERE category_id = ?)
		   ))`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE channel_id IN (
		   SELECT id FROM channels WHERE category_id = ?
		   UNION
		   SELECT id FROM channels WHERE parent_id IN (SELECT id FROM channels WHERE category_id = ?)
		 )`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_overwrites WHERE channel_id IN (
		   SELECT id FROM channels WHERE category_id = ?
		   UNION
		   SELECT id FROM channels WHERE parent_id IN (SELECT id FROM channels WHERE category_id = ?)
		 )`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channels WHERE parent_id IN (SELECT id FROM channels WHERE category_id = ?)`, id); err != nil {
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

// CreateMessage 同一事务写消息、@提及（#39）并认领附件（#41）：nonce 撞唯一索引
// 或附件认领失败时整体回滚，不会留下半条 message_mentions / 半认领的附件。
// 空 nonce 落 NULL，唯一索引上不受约束。
func (s *SQLite) CreateMessage(ctx context.Context, msg *Message) error {
	msg.ID = newID(msg.ID)
	claimIDs, err := normalizeClaimIDs(msg.AttachmentIDs)
	if err != nil {
		return err
	}
	var nonce any
	if msg.Nonce != "" {
		nonce = msg.Nonce
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content, nonce, reply_to) VALUES (?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.UserID, msg.Content, nonce, msg.ReplyTo); err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	// 全文索引与消息同事务写入（0016；SQLite 不用触发器，见迁移注释）。
	if err := ftsInsertMessage(ctx, tx, msg.ID, msg.Content); err != nil {
		return err
	}
	if err := insertMessageMentions(ctx, tx, msg.ID, normalizeMentions(msg.Mentions)); err != nil {
		return err
	}
	if err := claimAttachments(ctx, tx, msg.ID, msg.UserID, claimIDs); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id = ?`, msg.ID).
		Scan(&msg.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// sqMessageCols 列清单（nonce 经 COALESCE 归一空串）。
const sqMessageCols = `m.id, m.channel_id, m.user_id, m.content, COALESCE(m.nonce, ''), m.created_at,
	m.edited_at, m.deleted_at, m.reply_to`

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
		msg, err := scanMessage(rows, true)
		if err != nil {
			return nil, err
		}
		rev = append(rev, *msg)
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
		msg, err := scanMessage(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *msg)
	}
	return out, rows.Err()
}

// MessageByNonce 幂等回读原消息（带发送者内联信息，响应直接可用；频道维度隔离）。
func (s *SQLite) MessageByNonce(ctx context.Context, channelID, userID int64, nonce string) (*Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sqMessageCols+`, u.nickname, u.tag
		 FROM messages m JOIN users u ON u.id = m.user_id
		 WHERE m.channel_id = ? AND m.user_id = ? AND m.nonce = ?`, channelID, userID, nonce)
	msg, err := scanMessage(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// MessageByID 单条回读（#39，含软删行；发送者内联）。
func (s *SQLite) MessageByID(ctx context.Context, id int64) (*Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sqMessageCols+`, u.nickname, u.tag
		 FROM messages m JOIN users u ON u.id = m.user_id
		 WHERE m.id = ?`, id)
	msg, err := scanMessage(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// MessagesByIDs 批量回读（#39 回复预览）：一条 IN 查询，含软删行；不存在的 id 不出现。
func (s *SQLite) MessagesByIDs(ctx context.Context, ids []int64) (map[int64]*Message, error) {
	out := map[int64]*Message{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqMessageCols+`, u.nickname, u.tag
		 FROM messages m JOIN users u ON u.id = m.user_id
		 WHERE m.id IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		msg, err := scanMessage(rows, true)
		if err != nil {
			return nil, err
		}
		out[msg.ID] = msg
	}
	return out, rows.Err()
}

// MentionsOfMessages 批量查询提及用户（#39），组内按 user_id 升序；
// 没有提及的消息不出现，空入参不发 SQL。
func (s *SQLite) MentionsOfMessages(ctx context.Context, ids []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT message_id, user_id FROM message_mentions
		 WHERE message_id IN (`+placeholders(len(ids))+`)
		 ORDER BY message_id, user_id`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid, uid int64
		if err := rows.Scan(&mid, &uid); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], uid)
	}
	return out, rows.Err()
}

// UpdateMessageContent 同一事务更新正文/编辑时间、整体替换提及（#39）并同步
// 全文索引（#42）。软删行与不存在的 id 一律 ErrNotFound：墓碑不可再编辑。
func (s *SQLite) UpdateMessageContent(ctx context.Context, id int64, content string, mentions []int64, editedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 存在性与墓碑判定先于写：软删行与不存在的 id 一律 ErrNotFound。
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE id = ? AND deleted_at IS NULL`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// 索引项必须在改正文前删（外部内容表按当前正文反推词项），改完再写新词项。
	if err := ftsDeleteMessage(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET content = ?, edited_at = ? WHERE id = ? AND deleted_at IS NULL`,
		content, editedAt, id); err != nil {
		return err
	}
	if err := ftsInsertMessage(ctx, tx, id, content); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_mentions WHERE message_id = ?`, id); err != nil {
		return err
	}
	if err := insertMessageMentions(ctx, tx, id, normalizeMentions(mentions)); err != nil {
		return err
	}
	return tx.Commit()
}

// SoftDeleteMessage 软删（#39）：置 deleted_at、清空正文，行保留供回复引用；
// 同事务移除全文索引项（#42）。deleted_at IS NULL 条件让重复删除返回 ErrNotFound。
func (s *SQLite) SoftDeleteMessage(ctx context.Context, id int64, deletedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE id = ? AND deleted_at IS NULL`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// 先删索引项再清正文：FTS5 外部内容表按当前正文反推词项。
	if err := ftsDeleteMessage(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET deleted_at = ?, content = '' WHERE id = ? AND deleted_at IS NULL`,
		deletedAt, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- attachments（迁移 0015，#41 B1） ----

// CreateAttachment 写附件元数据；ID 未预置时取 Snowflake，created_at 回读数据库默认值。
func (s *SQLite) CreateAttachment(ctx context.Context, a *Attachment) error {
	if err := prepareAttachment(a); err != nil {
		return err
	}
	a.ID = newID(a.ID)
	var messageID any
	if a.MessageID != 0 {
		messageID = a.MessageID
	}
	if _, err := s.db.ExecContext(ctx, attachmentInsertSQL,
		a.ID, a.UploaderID, messageID, a.Filename, a.ContentType, a.Size,
		a.Width, a.Height, a.StorageKey, a.ThumbKey, a.AccessKey, a.SHA256); err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return s.db.QueryRowContext(ctx,
		`SELECT created_at FROM attachments WHERE id = ?`, a.ID).Scan(&a.CreatedAt)
}

// AttachmentByID 无命中 ErrNotFound。
func (s *SQLite) AttachmentByID(ctx context.Context, id int64) (*Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentCols+` FROM attachments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AttachmentByAccessKey 按下载能力凭证查找；无命中 ErrNotFound。
func (s *SQLite) AttachmentByAccessKey(ctx context.Context, accessKey string) (*Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentCols+` FROM attachments WHERE access_key = ?`, accessKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AttachmentsByMessageIDs 批量分组，组内按 id 升序；未认领与不存在的消息不出现。
func (s *SQLite) AttachmentsByMessageIDs(ctx context.Context, messageIDs []int64) (map[int64][]Attachment, error) {
	out := make(map[int64][]Attachment, len(messageIDs))
	if len(messageIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentCols+` FROM attachments
		  WHERE message_id IN (`+placeholders(len(messageIDs))+`) ORDER BY id ASC`,
		int64Args(messageIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out[a.MessageID] = append(out[a.MessageID], *a)
	}
	return out, rows.Err()
}

// OrphanAttachments 见 Store 接口；三类孤儿由共用 SQL 一次查出。
func (s *SQLite) OrphanAttachments(ctx context.Context, unclaimedBefore time.Time, limit int) ([]Attachment, error) {
	return queryOrphanAttachments(ctx, s.db, unclaimedBefore, limit)
}

// DeleteAttachmentRows 批量删除元数据行；不存在的 id 忽略，空入参 no-op。
func (s *SQLite) DeleteAttachmentRows(ctx context.Context, ids []int64) error {
	return deleteAttachmentRowsByIDs(ctx, s.db, ids)
}

// ---- read_states（迁移 0014，#40 B1） ----

// ReadState 返回该用户在该频道的已读水位；无记录 ErrNotFound。
func (s *SQLite) ReadState(ctx context.Context, userID, channelID int64) (int64, error) {
	var last int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_read_message_id FROM read_states WHERE user_id = ? AND channel_id = ?`,
		userID, channelID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return last, nil
}

// SetReadState 原子 upsert 并取 max：多设备乱序上报不会把水位拉回去。
// SQLite 没有 ON UPDATE CURRENT_TIMESTAMP，冲突分支显式刷新 updated_at。
func (s *SQLite) SetReadState(ctx context.Context, userID, channelID, messageID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO read_states (user_id, channel_id, last_read_message_id) VALUES (?, ?, ?)
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET
		     last_read_message_id = MAX(read_states.last_read_message_id, excluded.last_read_message_id),
		     updated_at = CURRENT_TIMESTAMP`,
		userID, channelID, messageID)
	return err
}

// EnsureReadState 仅补缺失行；已有水位绝不覆盖。
func (s *SQLite) EnsureReadState(ctx context.Context, userID, channelID, messageID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO read_states (user_id, channel_id, last_read_message_id) VALUES (?, ?, ?)
		 ON CONFLICT (user_id, channel_id) DO NOTHING`,
		userID, channelID, messageID)
	return err
}

// UnreadCounts 一条查询统计单用户在多个频道的未读/提及数；请求项全部出现在
// 结果里（无消息或已读完为 0）。参数顺序：频道 id 列表 → userID 三次（水位
// 关联、排除本人、提及关联）。
func (s *SQLite) UnreadCounts(ctx context.Context, userID int64, channelIDs []int64) (map[int64]Unread, error) {
	out := map[int64]Unread{}
	if len(channelIDs) == 0 {
		return out, nil
	}
	args := append(int64Args(channelIDs), userID, userID, userID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.channel_id, COUNT(msg.id), COUNT(mm.message_id)
		   FROM `+derivedUnion("channel_id", len(channelIDs))+` t
		   LEFT JOIN read_states rs ON rs.channel_id = t.channel_id AND rs.user_id = ?
		   LEFT JOIN messages msg ON msg.channel_id = t.channel_id
		        AND msg.id > COALESCE(rs.last_read_message_id, 0)
		        AND msg.user_id <> ?
		        AND msg.deleted_at IS NULL
		   LEFT JOIN message_mentions mm ON mm.message_id = msg.id AND mm.user_id = ?
		  GROUP BY t.channel_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var channelID int64
		var u Unread
		if err := rows.Scan(&channelID, &u.UnreadCount, &u.MentionCount); err != nil {
			return nil, err
		}
		out[channelID] = u
	}
	return out, rows.Err()
}

// UnreadCountsForUsers 一条查询统计某频道内多个用户的未读/提及数；没有
// read_states 行的用户按水位 0 计（其全部历史消息都算未读）。
// 参数顺序：用户 id 列表 → channelID 两次（水位关联、消息过滤）。
func (s *SQLite) UnreadCountsForUsers(ctx context.Context, channelID int64, userIDs []int64) (map[int64]Unread, error) {
	out := map[int64]Unread{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := append(int64Args(userIDs), channelID, channelID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.user_id, COUNT(msg.id), COUNT(mm.message_id)
		   FROM `+derivedUnion("user_id", len(userIDs))+` t
		   LEFT JOIN read_states rs ON rs.user_id = t.user_id AND rs.channel_id = ?
		   LEFT JOIN messages msg ON msg.channel_id = ?
		        AND msg.id > COALESCE(rs.last_read_message_id, 0)
		        AND msg.user_id <> t.user_id
		        AND msg.deleted_at IS NULL
		   LEFT JOIN message_mentions mm ON mm.message_id = msg.id AND mm.user_id = t.user_id
		  GROUP BY t.user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var userID int64
		var u Unread
		if err := rows.Scan(&userID, &u.UnreadCount, &u.MentionCount); err != nil {
			return nil, err
		}
		out[userID] = u
	}
	return out, rows.Err()
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

// ---- api_tokens（迁移 0018，#51 #54）----

const sqAPITokenCols = `id, user_id, name, prefix, secret_hash, kind, scopes, expires_at, last_used_at, revoked_at, created_at`

func (s *SQLite) CreateAPIToken(ctx context.Context, t *APIToken) error {
	t.ID = newID(t.ID)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (id, user_id, name, prefix, secret_hash, kind, scopes, expires_at, last_used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.Prefix, t.SecretHash, t.Kind, t.Scopes,
		t.ExpiresAt, t.LastUsedAt, t.RevokedAt)
	if err != nil {
		if isDupSQLite(err) {
			return ErrConflict
		}
		return err
	}
	return s.db.QueryRowContext(ctx, `SELECT created_at FROM api_tokens WHERE id = ?`, t.ID).
		Scan(&t.CreatedAt)
}

func (s *SQLite) scanAPIToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var exp, lastUsed, revoked sql.NullTime
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.SecretHash, &t.Kind,
		&t.Scopes, &exp, &lastUsed, &revoked, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exp.Valid {
		v := exp.Time
		t.ExpiresAt = &v
	}
	if lastUsed.Valid {
		v := lastUsed.Time
		t.LastUsedAt = &v
	}
	if revoked.Valid {
		v := revoked.Time
		t.RevokedAt = &v
	}
	return &t, nil
}

func (s *SQLite) APITokenByPrefix(ctx context.Context, prefix string) (*APIToken, error) {
	return s.scanAPIToken(s.db.QueryRowContext(ctx,
		`SELECT `+sqAPITokenCols+` FROM api_tokens WHERE prefix = ?`, prefix))
}

func (s *SQLite) APITokenByID(ctx context.Context, id int64) (*APIToken, error) {
	return s.scanAPIToken(s.db.QueryRowContext(ctx,
		`SELECT `+sqAPITokenCols+` FROM api_tokens WHERE id = ?`, id))
}

func (s *SQLite) APITokensOfUser(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqAPITokenCols+` FROM api_tokens WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := s.scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *SQLite) RevokeAPIToken(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// TouchAPITokenLastUsed 尽力而为：行已不存在（并发删除）不报错，异步路径不该失败。
func (s *SQLite) TouchAPITokenLastUsed(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, at, id)
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
