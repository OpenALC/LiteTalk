package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func emailKeySchemaError(detail string) error {
	return fmt.Errorf("邮箱规范键表结构不符: %s", detail)
}

// verifyEmailKeySchemaSQLite 不把 IF NOT EXISTS 的同名表误认为正确结构。
func verifyEmailKeySchemaSQLite(ctx context.Context, q emailKeyQueryer) error {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info('user_email_keys')`)
	if err != nil {
		return fmt.Errorf("邮箱规范键表结构检查失败: %s", safeEmailDBError(err))
	}
	type column struct {
		typ     string
		notNull int
		pk      int
	}
	cols := map[string]column{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键列读取失败: %s", safeEmailDBError(err))
		}
		cols[name] = column{typ: typ, notNull: notNull, pk: pk}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键列读取失败: %s", safeEmailDBError(err))
	}
	key, keyOK := cols["email_key"]
	user, userOK := cols["user_id"]
	if len(cols) != 2 || !keyOK || !userOK || !strings.EqualFold(key.typ, "BLOB") ||
		key.notNull != 1 || key.pk != 1 || !strings.EqualFold(user.typ, "INTEGER") ||
		user.notNull != 1 || user.pk != 0 {
		return emailKeySchemaError("SQLite 列类型/非空/主键")
	}

	rows, err = q.QueryContext(ctx, `PRAGMA index_list('user_email_keys')`)
	if err != nil {
		return fmt.Errorf("邮箱规范键索引检查失败: %s", safeEmailDBError(err))
	}
	var uniqueIndexes []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键索引读取失败: %s", safeEmailDBError(err))
		}
		if unique == 1 && partial == 0 {
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键索引读取失败: %s", safeEmailDBError(err))
	}
	uniqueUser := false
	for _, index := range uniqueIndexes {
		quoted := strings.ReplaceAll(index, "'", "''")
		idxRows, err := q.QueryContext(ctx, `PRAGMA index_info('`+quoted+`')`)
		if err != nil {
			return fmt.Errorf("邮箱规范键索引检查失败: %s", safeEmailDBError(err))
		}
		var indexed []string
		for idxRows.Next() {
			var seqNo, cid int
			var name string
			if err := idxRows.Scan(&seqNo, &cid, &name); err != nil {
				_ = idxRows.Close()
				return fmt.Errorf("邮箱规范键索引读取失败: %s", safeEmailDBError(err))
			}
			indexed = append(indexed, name)
		}
		err = idxRows.Err()
		_ = idxRows.Close()
		if err != nil {
			return fmt.Errorf("邮箱规范键索引读取失败: %s", safeEmailDBError(err))
		}
		if len(indexed) == 1 && indexed[0] == "user_id" {
			uniqueUser = true
		}
	}
	if !uniqueUser {
		return emailKeySchemaError("SQLite user_id 单列唯一键")
	}

	rows, err = q.QueryContext(ctx, `PRAGMA foreign_key_list('user_email_keys')`)
	if err != nil {
		return fmt.Errorf("邮箱规范键外键检查失败: %s", safeEmailDBError(err))
	}
	fkCount := 0
	fkOK := false
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键外键读取失败: %s", safeEmailDBError(err))
		}
		fkCount++
		fkOK = seq == 0 && table == "users" && from == "user_id" && to == "id" &&
			strings.EqualFold(onDelete, "CASCADE")
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键外键读取失败: %s", safeEmailDBError(err))
	}
	if fkCount != 1 || !fkOK {
		return emailKeySchemaError("SQLite user_id 外键级联")
	}
	return nil
}

func verifyEmailKeySchemaMySQL(ctx context.Context, q emailKeyQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT column_name, data_type, is_nullable, character_maximum_length
		FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'user_email_keys'`)
	if err != nil {
		return fmt.Errorf("邮箱规范键列检查失败: %s", safeEmailDBError(err))
	}
	type column struct {
		typ    string
		null   string
		length sql.NullInt64
	}
	cols := map[string]column{}
	for rows.Next() {
		var name, typ, nullable string
		var length sql.NullInt64
		if err := rows.Scan(&name, &typ, &nullable, &length); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键列读取失败: %s", safeEmailDBError(err))
		}
		cols[name] = column{typ: typ, null: nullable, length: length}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键列读取失败: %s", safeEmailDBError(err))
	}
	key, keyOK := cols["email_key"]
	user, userOK := cols["user_id"]
	if len(cols) != 2 || !keyOK || !userOK || key.typ != "varbinary" || key.null != "NO" ||
		!key.length.Valid || key.length.Int64 != maxEmailKeyBytes || user.typ != "bigint" || user.null != "NO" {
		return emailKeySchemaError("MySQL 二进制列/长度/非空")
	}

	rows, err = q.QueryContext(ctx, `SELECT index_name, non_unique, seq_in_index, column_name, sub_part
		FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'user_email_keys'
		ORDER BY index_name, seq_in_index`)
	if err != nil {
		return fmt.Errorf("邮箱规范键索引检查失败: %s", safeEmailDBError(err))
	}
	type index struct {
		unique bool
		cols   []string
		part   bool
	}
	indexes := map[string]index{}
	for rows.Next() {
		var name, col string
		var nonUnique, seq int
		var part sql.NullInt64
		if err := rows.Scan(&name, &nonUnique, &seq, &col, &part); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键索引读取失败: %s", safeEmailDBError(err))
		}
		idx := indexes[name]
		idx.unique = nonUnique == 0
		idx.cols = append(idx.cols, col)
		idx.part = idx.part || part.Valid
		indexes[name] = idx
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键索引读取失败: %s", safeEmailDBError(err))
	}
	primary := indexes["PRIMARY"]
	if !primary.unique || primary.part || len(primary.cols) != 1 || primary.cols[0] != "email_key" {
		return emailKeySchemaError("MySQL email_key 完整主键")
	}
	uniqueUser := false
	for _, idx := range indexes {
		if idx.unique && !idx.part && len(idx.cols) == 1 && idx.cols[0] == "user_id" {
			uniqueUser = true
		}
	}
	if !uniqueUser {
		return emailKeySchemaError("MySQL user_id 单列唯一键")
	}

	rows, err = q.QueryContext(ctx, `SELECT k.column_name, k.referenced_table_name, k.referenced_column_name, r.delete_rule
		FROM information_schema.key_column_usage k JOIN information_schema.referential_constraints r
		ON r.constraint_schema = k.constraint_schema AND r.table_name = k.table_name
		AND r.constraint_name = k.constraint_name
		WHERE k.table_schema = DATABASE() AND k.table_name = 'user_email_keys'
		AND k.referenced_table_name IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("邮箱规范键外键检查失败: %s", safeEmailDBError(err))
	}
	fkCount := 0
	fkOK := false
	for rows.Next() {
		var from, table, to, deleteRule string
		if err := rows.Scan(&from, &table, &to, &deleteRule); err != nil {
			_ = rows.Close()
			return fmt.Errorf("邮箱规范键外键读取失败: %s", safeEmailDBError(err))
		}
		fkCount++
		fkOK = from == "user_id" && table == "users" && to == "id" &&
			strings.EqualFold(deleteRule, "CASCADE")
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("邮箱规范键外键读取失败: %s", safeEmailDBError(err))
	}
	if fkCount != 1 || !fkOK {
		return emailKeySchemaError("MySQL user_id 外键级联")
	}
	return nil
}
