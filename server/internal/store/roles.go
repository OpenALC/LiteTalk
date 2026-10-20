package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// 迁移 0010（#35 B1）共用逻辑：排序口径、默认角色 key、SQL 行扫描与方言无关的
// 角色域 SQL。MySQL/SQLite 在该域的差异只剩 SetChannelOverwrite 的 upsert 语法
// 与唯一冲突判定（作为参数传入），其余共用一份实现，避免两侧语义漂移。

// sqlExecer *sql.DB 与 *sql.Tx 的公共面。
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// defaultRoleKeys 成员入会要分配的 builtin key：@everyone + 按 legacy role 追加。
func defaultRoleKeys(legacy MemberRole) []string {
	keys := []string{BuiltinKeyMember}
	switch legacy {
	case RoleAdmin:
		keys = append(keys, BuiltinKeyAdmin)
	case RoleOwner:
		keys = append(keys, BuiltinKeyOwner)
	}
	return keys
}

// sortRoles 统一排序：position、id 升序（三实现同口径）。
func sortRoles(list []Role) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Position != list[j].Position {
			return list[i].Position < list[j].Position
		}
		return list[i].ID < list[j].ID
	})
}

// sortOverwrites 统一排序：withChannel 时先 channel_id，再 target_type、target_id。
// 不用 SQL ORDER BY target_type：MySQL 列是 ENUM（按定义序）而 SQLite 是 TEXT
// （按字典序），'member'/'role' 两种序恰好相反，必须在 Go 侧定序。
func sortOverwrites(list []ChannelOverwrite, withChannel bool) {
	sort.Slice(list, func(i, j int) bool {
		if withChannel && list[i].ChannelID != list[j].ChannelID {
			return list[i].ChannelID < list[j].ChannelID
		}
		if list[i].TargetType != list[j].TargetType {
			return list[i].TargetType < list[j].TargetType
		}
		return list[i].TargetID < list[j].TargetID
	})
}

const roleCols = `id, server_id, name, color, position, permissions, builtin_key, created_at, updated_at`

// scanRole 扫描一行角色。permissions 先扫 int64 再转（DB 列是有符号 BIGINT）；
// builtin_key 可空，空串表示自定义角色。
func scanRole(row interface{ Scan(...any) error }) (*Role, error) {
	var r Role
	var perms int64
	var builtin sql.NullString
	err := row.Scan(&r.ID, &r.ServerID, &r.Name, &r.Color, &r.Position, &perms,
		&builtin, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Permissions = Permission(perms)
	if builtin.Valid {
		r.BuiltinKey = builtin.String
	}
	return &r, nil
}

const overwriteCols = `channel_id, target_type, target_id, allow, deny, updated_at`

// scanOverwrite 扫描一行频道覆盖。
func scanOverwrite(row interface{ Scan(...any) error }) (*ChannelOverwrite, error) {
	var o ChannelOverwrite
	var allow, deny int64
	err := row.Scan(&o.ChannelID, &o.TargetType, &o.TargetID, &allow, &deny, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Allow, o.Deny = Permission(allow), Permission(deny)
	return &o, nil
}

const auditCols = `id, server_id, actor_id, action, target_type, target_id, channel_id, detail, created_at`

// scanAuditLog 扫描一行审计。target_type/target_id/channel_id 均可空。
func scanAuditLog(row interface{ Scan(...any) error }) (*AuditLog, error) {
	var a AuditLog
	var targetType sql.NullString
	var targetID, channelID sql.NullInt64
	err := row.Scan(&a.ID, &a.ServerID, &a.ActorID, &a.Action, &targetType,
		&targetID, &channelID, &a.Detail, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if targetType.Valid {
		a.TargetType = targetType.String
	}
	if targetID.Valid {
		v := targetID.Int64
		a.TargetID = &v
	}
	if channelID.Valid {
		v := channelID.Int64
		a.ChannelID = &v
	}
	return &a, nil
}

// dbCreateRole 写自定义角色。builtin_key 恒 NULL（builtin 由建服/迁移回填写），
// 显式时间列保证返回值与 Mem 同为 Go 侧时钟。
func dbCreateRole(ctx context.Context, q sqlExecer, r *Role, isDup func(error) bool) error {
	if r.BuiltinKey != "" {
		return ErrForbidden
	}
	if !r.Permissions.Valid() {
		return ErrForbidden
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE id = ?`, r.ServerID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	r.ID = newID(r.ID)
	now := time.Now()
	if r.Color == "" {
		r.Color = DefaultRoleColor
	}
	r.CreatedAt, r.UpdatedAt = now, now
	if _, err := q.ExecContext(ctx,
		`INSERT INTO roles (id, server_id, name, color, position, permissions, builtin_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		r.ID, r.ServerID, r.Name, r.Color, r.Position, int64(r.Permissions), r.CreatedAt, r.UpdatedAt); err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func dbListRoles(ctx context.Context, q sqlExecer, serverID int64) ([]Role, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+roleCols+` FROM roles WHERE server_id = ? ORDER BY position, id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Role{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func dbRoleByID(ctx context.Context, q sqlExecer, id int64) (*Role, error) {
	return scanRole(q.QueryRowContext(ctx, `SELECT `+roleCols+` FROM roles WHERE id = ?`, id))
}

// dbUpdateRole 全量写 name/color/position/permissions；MySQL 无 clientFoundRows 时
// 值全同会报 0 行，存在即成功（与 Mem 对齐），不存在才 ErrNotFound。
func dbUpdateRole(ctx context.Context, q sqlExecer, r *Role) error {
	old, err := scanRole(q.QueryRowContext(ctx, `SELECT `+roleCols+` FROM roles WHERE id = ?`, r.ID))
	if err != nil {
		return err
	}
	if old.IsBuiltin() {
		return ErrForbidden
	}
	if !r.Permissions.Valid() {
		return ErrForbidden
	}
	color := r.Color
	if color == "" {
		color = DefaultRoleColor
	}
	res, err := q.ExecContext(ctx,
		`UPDATE roles SET name = ?, color = ?, position = ?, permissions = ?, updated_at = ?
		  WHERE id = ? AND builtin_key IS NULL`,
		r.Name, color, r.Position, int64(r.Permissions), time.Now(), r.ID)
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
	var id int64
	err = q.QueryRowContext(ctx, `SELECT id FROM roles WHERE id = ?`, r.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// dbDeleteRole 删自定义角色并级联分配与以它为目标的覆盖（调用方开事务）。
func dbDeleteRole(ctx context.Context, tx *sql.Tx, id int64) error {
	old, err := scanRole(tx.QueryRowContext(ctx, `SELECT `+roleCols+` FROM roles WHERE id = ?`, id))
	if err != nil {
		return err
	}
	if old.IsBuiltin() {
		return ErrForbidden
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM member_roles WHERE role_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_overwrites WHERE target_type = 'role' AND target_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM roles WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func dbRolesOfMember(ctx context.Context, q sqlExecer, serverID, userID int64) ([]Role, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_members WHERE server_id = ? AND user_id = ?`, serverID, userID).
		Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	// 列名带 r. 前缀：member_roles 也有 server_id，裸列名在 JOIN 里两方言都报歧义。
	rows, err := q.QueryContext(ctx,
		`SELECT r.id, r.server_id, r.name, r.color, r.position, r.permissions,
		        r.builtin_key, r.created_at, r.updated_at
		   FROM roles r
		   JOIN member_roles mr ON mr.role_id = r.id
		  WHERE mr.server_id = ? AND mr.user_id = ?`, serverID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Role{}
	seen := map[int64]bool{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
		seen[r.ID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// @everyone 恒在结果里：分配行缺失（历史数据/损坏）也不能丢，权限基线靠它。
	var everyoneID int64
	err = q.QueryRowContext(ctx,
		`SELECT id FROM roles WHERE server_id = ? AND builtin_key = ?`, serverID, BuiltinKeyMember).
		Scan(&everyoneID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && !seen[everyoneID] {
		r, err := scanRole(q.QueryRowContext(ctx, `SELECT `+roleCols+` FROM roles WHERE id = ?`, everyoneID))
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	sortRoles(out)
	return out, nil
}

// dbSetMemberRoles 整组替换分配（调用方开事务）：校验同服、强制保留 @everyone、
// owner 强制保留 owner builtin，并把 server_members.role 投影同步回去。
func dbSetMemberRoles(ctx context.Context, tx *sql.Tx, serverID, userID int64, roleIDs []int64, assignedBy int64) error {
	var legacy MemberRole
	err := tx.QueryRowContext(ctx,
		`SELECT role FROM server_members WHERE server_id = ? AND user_id = ?`, serverID, userID).Scan(&legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var ownerUID int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = ?`, serverID).
		Scan(&ownerUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+roleCols+` FROM roles WHERE server_id = ?`, serverID)
	if err != nil {
		return err
	}
	byID := map[int64]*Role{}
	var everyoneID, adminID, ownerID int64
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			rows.Close()
			return err
		}
		byID[r.ID] = r
		switch r.BuiltinKey {
		case BuiltinKeyMember:
			everyoneID = r.ID
		case BuiltinKeyAdmin:
			adminID = r.ID
		case BuiltinKeyOwner:
			ownerID = r.ID
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if everyoneID == 0 {
		return ErrNotFound // 数据损坏：@everyone 缺失（与 Mem 同码）
	}
	desired := map[int64]bool{}
	for _, id := range roleIDs {
		if _, ok := byID[id]; !ok {
			return ErrNotFound // 不存在或跨服，统一不出存在性
		}
		desired[id] = true
	}
	desired[everyoneID] = true
	// owner 强制保留 owner 投影：服主或 legacy owner 都不可被摘掉 owner builtin。
	if ownerUID == userID || legacy == RoleOwner {
		if ownerID == 0 {
			return ErrNotFound // 数据损坏：owner 角色缺失（与 Mem 同码）
		}
		desired[ownerID] = true
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM member_roles WHERE server_id = ? AND user_id = ?`, serverID, userID); err != nil {
		return err
	}
	now := time.Now()
	for id := range desired {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO member_roles (server_id, user_id, role_id, assigned_at, assigned_by) VALUES (?, ?, ?, ?, ?)`,
			serverID, userID, id, now, assignedBy); err != nil {
			return err
		}
	}
	// legacy 列是投影：owner→owner；有 admin→admin；否则 member。
	newLegacy := RoleMember
	switch {
	case ownerID != 0 && desired[ownerID]:
		newLegacy = RoleOwner
	case adminID != 0 && desired[adminID]:
		newLegacy = RoleAdmin
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE server_members SET role = ? WHERE server_id = ? AND user_id = ?`, newLegacy, serverID, userID)
	return err
}

// dbRoleAssignmentsOfServer 全服分配，按 user_id、role_id 升序。
func dbRoleAssignmentsOfServer(ctx context.Context, q sqlExecer, serverID int64) ([]MemberRoleAssignment, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT server_id, user_id, role_id, assigned_at, assigned_by
		   FROM member_roles WHERE server_id = ? ORDER BY user_id, role_id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemberRoleAssignment{}
	for rows.Next() {
		var a MemberRoleAssignment
		if err := rows.Scan(&a.ServerID, &a.UserID, &a.RoleID, &a.AssignedAt, &a.AssignedBy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// dbAssignDefaultRoles 写 @everyone + legacy admin/owner 的 builtin 分配
// （调用方开事务；缺 builtin 直接失败整事务回滚）。isDup 随方言传入。
func dbAssignDefaultRoles(ctx context.Context, tx *sql.Tx, serverID, userID int64, legacy MemberRole, assignedBy int64, at time.Time, isDup func(error) bool) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, builtin_key FROM roles
		  WHERE server_id = ? AND builtin_key IN (?, ?, ?)`,
		serverID, BuiltinKeyMember, BuiltinKeyAdmin, BuiltinKeyOwner)
	if err != nil {
		return err
	}
	found := map[string]int64{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return err
		}
		found[key] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, key := range defaultRoleKeys(legacy) {
		roleID, ok := found[key]
		if !ok {
			return ErrNotFound // builtin 缺失属数据损坏，与 Mem 同码
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO member_roles (server_id, user_id, role_id, assigned_at, assigned_by) VALUES (?, ?, ?, ?, ?)`,
			serverID, userID, roleID, at, assignedBy); err != nil {
			if isDup(err) {
				return ErrConflict
			}
			return err
		}
	}
	return nil
}

// dbAddMemberWithDefaults 插成员 + 默认角色（调用方开事务；冲突不写分配）。
func dbAddMemberWithDefaults(ctx context.Context, tx *sql.Tx, mem *ServerMember, isDup func(error) bool) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE id = ?`, mem.ServerID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if mem.JoinedAt.IsZero() {
		mem.JoinedAt = time.Now()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_members (server_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`,
		mem.ServerID, mem.UserID, mem.Role, mem.JoinedAt); err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return dbAssignDefaultRoles(ctx, tx, mem.ServerID, mem.UserID, mem.Role, 0, mem.JoinedAt, isDup)
}

func dbChannelOverwrites(ctx context.Context, q sqlExecer, channelID int64) ([]ChannelOverwrite, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+overwriteCols+` FROM channel_overwrites WHERE channel_id = ?`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelOverwrite{}
	for rows.Next() {
		o, err := scanOverwrite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortOverwrites(out, false)
	return out, nil
}

func dbChannelOverwritesOfServer(ctx context.Context, q sqlExecer, serverID int64) ([]ChannelOverwrite, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+overwriteCols+` FROM channel_overwrites o
		  JOIN channels c ON c.id = o.channel_id
		 WHERE c.server_id = ?`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelOverwrite{}
	for rows.Next() {
		o, err := scanOverwrite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortOverwrites(out, true)
	return out, nil
}

// dbOverwriteTargetOK 校验 target_type 合法、频道存在、目标（角色/成员）同服。
// 不出跨服存在性：目标不属于该服一律 ErrNotFound。
func dbOverwriteTargetOK(ctx context.Context, q sqlExecer, o *ChannelOverwrite) error {
	if o.TargetType != OverwriteTargetRole && o.TargetType != OverwriteTargetMember {
		return ErrForbidden
	}
	if !o.Allow.Valid() || !o.Deny.Valid() {
		return ErrForbidden
	}
	var serverID int64
	if err := q.QueryRowContext(ctx, `SELECT server_id FROM channels WHERE id = ?`, o.ChannelID).
		Scan(&serverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	switch o.TargetType {
	case OverwriteTargetRole:
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM roles WHERE id = ? AND server_id = ?`, o.TargetID, serverID).
			Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	case OverwriteTargetMember:
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM server_members WHERE server_id = ? AND user_id = ?`, serverID, o.TargetID).
			Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	return nil
}

func dbDeleteChannelOverwrite(ctx context.Context, q sqlExecer, channelID int64, targetType OverwriteTargetType, targetID int64) error {
	res, err := q.ExecContext(ctx,
		`DELETE FROM channel_overwrites WHERE channel_id = ? AND target_type = ? AND target_id = ?`,
		channelID, targetType, targetID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// dbWriteAudit 落审计行，回填 ID/时间。可空列显式给 nil（空串 ≠ NULL）。
func dbWriteAudit(ctx context.Context, q sqlExecer, a *AuditLog, isDup func(error) bool) error {
	a.ID = newID(a.ID)
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	var targetType any
	if a.TargetType != "" {
		targetType = a.TargetType
	}
	var targetID any
	if a.TargetID != nil {
		targetID = *a.TargetID
	}
	var channelID any
	if a.ChannelID != nil {
		channelID = *a.ChannelID
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO audit_log (id, server_id, actor_id, action, target_type, target_id, channel_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ServerID, a.ActorID, a.Action, targetType, targetID, channelID, a.Detail, a.CreatedAt); err != nil {
		if isDup(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

// dbAuditLogBefore 倒序游标（新→旧）：beforeID=0 取最新 limit 条。
func dbAuditLogBefore(ctx context.Context, q sqlExecer, serverID, beforeID int64, limit int) ([]AuditLog, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = q.QueryContext(ctx,
			`SELECT `+auditCols+` FROM audit_log
			  WHERE server_id = ? AND id < ? ORDER BY id DESC LIMIT ?`, serverID, beforeID, limit)
	} else {
		rows, err = q.QueryContext(ctx,
			`SELECT `+auditCols+` FROM audit_log
			  WHERE server_id = ? ORDER BY id DESC LIMIT ?`, serverID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditLog{}
	for rows.Next() {
		a, err := scanAuditLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
