-- 0011_channel_options.up.sql — 频道密码、人数上限、子频道树（#37 B1）
-- 三列一索引放在同一条 ALTER 里：MySQL 的多子句 ALTER 是原子 DDL，
-- 避免「加了两列断电」的半应用状态（迁移失败不会写 schema_migrations，重跑即续）。
-- 旧行默认：无密码（''）/ 不限人数（0）/ 顶层（NULL）。
ALTER TABLE channels
    ADD COLUMN password_hash VARCHAR(255) NOT NULL DEFAULT '',
    ADD COLUMN user_limit INT NOT NULL DEFAULT 0,
    ADD COLUMN parent_id BIGINT NULL,
    ADD INDEX idx_channels_parent (parent_id);
