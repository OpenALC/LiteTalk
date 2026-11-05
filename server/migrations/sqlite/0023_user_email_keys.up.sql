-- 0023_user_email_keys.up.sql — 与 MySQL 同口径的二进制 Go 邮箱规范键。
-- 回填和版本标记由 Store 迁移器在同一事务提交，旧邮箱原值不变。
CREATE TABLE IF NOT EXISTS user_email_keys (
    email_key BLOB NOT NULL PRIMARY KEY,
    user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE
) WITHOUT ROWID;
