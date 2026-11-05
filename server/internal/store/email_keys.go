package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/OpenALC/LiteTalk/server/internal/emailnorm"
	mysqldrv "github.com/go-sql-driver/mysql"
	"modernc.org/sqlite"
)

const emailKeysVersion = "0023_user_email_keys"
const maxEmailKeyBytes = 1020

// ErrEmailKeyConflict 表示旧账号按现行邮箱规则归一后相同；迁移不改任何账号。
var ErrEmailKeyConflict = errors.New("邮箱规范键冲突")

type emailKeyRow struct {
	id  int64
	key []byte
}

type emailKeyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type emailKeyExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// collectEmailKeys 仅扫描，不写入。错误消息只带内部 ID，不带邮箱或规范键。
func collectEmailKeys(ctx context.Context, q emailKeyQueryer) ([]emailKeyRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, email FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("邮箱预检查询失败: %s", safeEmailDBError(err))
	}
	defer rows.Close()
	var all []emailKeyRow
	groups := make(map[string][]int64)
	for rows.Next() {
		var id int64
		var email string
		if err := rows.Scan(&id, &email); err != nil {
			return nil, fmt.Errorf("邮箱预检读取失败: %s", safeEmailDBError(err))
		}
		key := emailnorm.Normalize(email)
		if len(key) > maxEmailKeyBytes {
			return nil, fmt.Errorf("邮箱规范键超长，用户 ID: %d", id)
		}
		groups[key] = append(groups[key], id)
		all = append(all, emailKeyRow{id: id, key: []byte(key)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("邮箱预检读取失败: %s", safeEmailDBError(err))
	}
	var conflicts [][]int64
	for _, ids := range groups {
		if len(ids) > 1 {
			conflicts = append(conflicts, ids)
		}
	}
	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i][0] < conflicts[j][0] })
		return nil, fmt.Errorf("%w，用户 ID 组: %v", ErrEmailKeyConflict, conflicts)
	}
	return all, nil
}

func insertEmailKeys(ctx context.Context, q emailKeyExecer, rows []emailKeyRow) error {
	for _, row := range rows {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO user_email_keys (email_key, user_id) VALUES (?, ?)`, row.key, row.id); err != nil {
			return fmt.Errorf("回填邮箱规范键失败，用户 ID: %d，%s", row.id, safeEmailDBError(err))
		}
	}
	return nil
}

// verifyEmailKeyRows 比对每个用户的 Go 规范键；错误只含内部 ID。
func verifyEmailKeyRows(ctx context.Context, q emailKeyQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT u.id, u.email, k.user_id, k.email_key
		FROM users u LEFT JOIN user_email_keys k ON k.user_id = u.id ORDER BY u.id`)
	if err != nil {
		return fmt.Errorf("邮箱规范键行检查失败: %s", safeEmailDBError(err))
	}
	var users int64
	for rows.Next() {
		var id int64
		var email string
		var keyUser sql.NullInt64
		var key []byte
		if err := rows.Scan(&id, &email, &keyUser, &key); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键行读取失败: %s", safeEmailDBError(err))
		}
		if !keyUser.Valid || !bytes.Equal(key, []byte(emailnorm.Normalize(email))) {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键缺失或错误，用户 ID: %d", id)
		}
		users++
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键行读取失败: %s", safeEmailDBError(err))
	}
	var keys int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_email_keys`).Scan(&keys); err != nil {
		return fmt.Errorf("邮箱规范键计数失败: %s", safeEmailDBError(err))
	}
	if keys != users {
		return fmt.Errorf("邮箱规范键行数不符: users=%d keys=%d", users, keys)
	}
	return nil
}

func safeEmailDBError(err error) string {
	var my *mysqldrv.MySQLError
	if errors.As(err, &my) {
		if my.SQLState != [5]byte{} {
			return fmt.Sprintf("mysql errno=%d sqlstate=%s", my.Number, string(my.SQLState[:]))
		}
		return fmt.Sprintf("mysql errno=%d", my.Number)
	}
	var sq *sqlite.Error
	if errors.As(err, &sq) {
		return fmt.Sprintf("sqlite code=%d", sq.Code())
	}
	return fmt.Sprintf("type=%T", err)
}

func emailKeysApplied(ctx context.Context, q emailKeyQueryer) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, emailKeysVersion).Scan(&n)
	return n != 0, err
}

func createUserWithEmailKey(ctx context.Context, db *sql.DB, u *User, isDuplicate func(error) bool) error {
	key := emailnorm.Normalize(u.Email)
	if len(key) > maxEmailKeyBytes {
		return errors.New("邮箱规范键超长")
	}
	u.ID = newID(u.ID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash, email_verified, is_guest, is_bot, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Nickname, u.Tag, u.PasswordHash, u.EmailVerified, u.IsGuest, u.IsBot, u.Status)
	if err != nil {
		if isDuplicate(err) {
			return ErrConflict
		}
		return fmt.Errorf("创建用户失败，用户 ID: %d，%s", u.ID, safeEmailDBError(err))
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_email_keys (email_key, user_id) VALUES (?, ?)`, []byte(key), u.ID); err != nil {
		if isDuplicate(err) {
			return ErrConflict
		}
		return fmt.Errorf("创建邮箱规范键失败，用户 ID: %d，%s", u.ID, safeEmailDBError(err))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM users WHERE id = ?`, u.ID).
		Scan(&u.CreatedAt, &u.UpdatedAt)
}
