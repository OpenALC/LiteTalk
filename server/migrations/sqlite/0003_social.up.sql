-- 0003_social.up.sql — 好友备注/分组 + 频道分类（SQLite 方言：无 AFTER，ADD KEY 写成独立 CREATE INDEX）
ALTER TABLE friendships ADD COLUMN remark     VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE friendships ADD COLUMN group_name VARCHAR(32) NOT NULL DEFAULT '';

-- 频道分类：独立表（空分类可持久化），频道经 category_id 归属（NULL=未分类）
CREATE TABLE categories (
    id         INTEGER     PRIMARY KEY,
    server_id  INTEGER     NOT NULL,
    name       VARCHAR(32) NOT NULL,
    position   INT         NOT NULL DEFAULT 0,
    created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_categories_server ON categories (server_id);

ALTER TABLE channels ADD COLUMN category_id BIGINT NULL;
CREATE INDEX idx_channels_category ON channels (category_id);
