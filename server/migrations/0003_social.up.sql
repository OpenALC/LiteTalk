-- 0003_social.up.sql — 好友备注/分组 + 频道分类（v0.1.2 走查需求 #3/#8）
ALTER TABLE friendships ADD COLUMN remark     VARCHAR(64) NOT NULL DEFAULT '' AFTER status;
ALTER TABLE friendships ADD COLUMN group_name VARCHAR(32) NOT NULL DEFAULT '' AFTER remark;

-- 频道分类：独立表（空分类可持久化），频道经 category_id 归属（NULL=未分类）
CREATE TABLE categories (
    id         BIGINT      NOT NULL AUTO_INCREMENT,
    server_id  BIGINT      NOT NULL,
    name       VARCHAR(32) NOT NULL,
    position   INT         NOT NULL DEFAULT 0,
    created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_categories_server (server_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE channels ADD COLUMN category_id BIGINT NULL AFTER type;
ALTER TABLE channels ADD KEY idx_channels_category (category_id);
