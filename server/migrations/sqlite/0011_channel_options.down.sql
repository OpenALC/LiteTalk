-- 0011_channel_options.down.sql — 先删索引（DROP COLUMN 不接受列被索引引用），
-- 再逆序删列。SQLite 的 DROP COLUMN 自 3.35 起可用（modernc 驱动远高于此）。
DROP INDEX idx_channels_parent;
ALTER TABLE channels DROP COLUMN parent_id;
ALTER TABLE channels DROP COLUMN user_limit;
ALTER TABLE channels DROP COLUMN password_hash;
