-- 0003_social.down.sql — 逆序回滚
ALTER TABLE channels DROP KEY idx_channels_category;
ALTER TABLE channels DROP COLUMN category_id;
DROP TABLE categories;
ALTER TABLE friendships DROP COLUMN group_name;
ALTER TABLE friendships DROP COLUMN remark;
