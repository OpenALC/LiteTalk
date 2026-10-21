-- 0011_channel_options.up.sql — 频道密码、人数上限、子频道树（#37 B1；SQLite 方言）
-- SQLite 无多子句 ALTER，逐列 ADD + 独立建索引（整轮在事务内，失败不落记账）。
-- 旧行默认：无密码（''）/ 不限人数（0）/ 顶层（NULL）。
ALTER TABLE channels ADD COLUMN password_hash VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN user_limit INT NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN parent_id BIGINT NULL;
CREATE INDEX idx_channels_parent ON channels (parent_id);
