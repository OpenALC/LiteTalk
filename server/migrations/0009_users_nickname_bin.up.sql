-- 0009_users_nickname_bin.up.sql — nickname 统一为大小写敏感（#24 P2）
-- 旧列继承表默认的 utf8mb4_unicode_ci：唯一索引 uk_users_nickname_tag 与
-- UserByNicknameTag 都把 Alice 与 alice 当作同名，而 Mem/SQLite 一直是逐字节敏感。
-- 改列为 utf8mb4_bin，唯一索引随列重建，三种 Store 语义对齐。
--
-- 重复行评估：旧唯一索引本身就是大小写不敏感的，(nickname, tag) 上不可能同时
-- 存在仅大小写不同的两行——正常维护的库不会在重建索引时失败。只有索引被手工
-- 删过才可能；升级前可执行下面的检查（有输出说明必须先处理）：
--   SELECT GROUP_CONCAT(id ORDER BY id) AS ids, MIN(nickname) AS nickname, tag,
--          COUNT(*) AS n
--     FROM users
--    GROUP BY nickname COLLATE utf8mb4_bin, tag
--   HAVING COUNT(*) > 1;
-- 处理策略 fail-closed：不自动删除、不改 tag——大小写变体在旧语义下仍是独立
-- 账号，自动改 tag 会让既有「昵称#tag」寻址失效。由运维导出冲突行、人工改名
-- 后重跑本迁移；失败的迁移不写 schema_migrations，重启会自动重试。
ALTER TABLE users
    MODIFY nickname VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL;
