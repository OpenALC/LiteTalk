-- 0023_user_email_keys.up.sql — Go 邮箱规范键，原 users.email 保持不变。
-- 回填和版本标记由 Store 迁移器在同一事务提交；重复账号在建表前拒绝。
CREATE TABLE IF NOT EXISTS user_email_keys (
    email_key VARBINARY(1020) NOT NULL,
    user_id BIGINT NOT NULL,
    PRIMARY KEY (email_key),
    UNIQUE KEY uk_user_email_keys_user (user_id),
    CONSTRAINT fk_user_email_keys_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB;
