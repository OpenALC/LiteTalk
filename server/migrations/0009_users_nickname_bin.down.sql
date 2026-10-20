-- 0009_users_nickname_bin.down.sql — 昵称回滚为大小写不敏感（#24 P2）
-- 若升级后已产生仅大小写不同的「昵称#tag」（如 Alice#0001 与 alice#0001），
-- 改回 unicode_ci 时唯一索引重建会报 Duplicate entry，回滚失败；需先人工
-- 处理这些行。失败时 schema_migrations 保留 0009，可处理后重试。
ALTER TABLE users
    MODIFY nickname VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL;
