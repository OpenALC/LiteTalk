-- 0003_social.down.sql — 逆序回滚（DROP COLUMN 前先删该列上的索引，否则 SQLite 拒绝）
DROP INDEX IF EXISTS idx_channels_category;
ALTER TABLE channels DROP COLUMN category_id;
DROP TABLE IF EXISTS categories;
ALTER TABLE friendships DROP COLUMN group_name;
ALTER TABLE friendships DROP COLUMN remark;
