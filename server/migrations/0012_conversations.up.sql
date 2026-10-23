-- 0012_conversations.up.sql — 私聊/群私聊建模为无服务器频道（#38 B1）
-- server_id 可 NULL：direct/group 没有所属服务器；scope 区分三类频道。
-- dm_key 只有 direct 有值（"<minUID>:<maxUID>"），唯一索引兜住并发重复建会话
-- （MySQL 唯一索引允许多个 NULL，server/group 行不受影响）。
-- 多子句 ALTER 是原子 DDL，避免「加了一半断电」的半应用状态。
ALTER TABLE channels
    MODIFY COLUMN server_id BIGINT NULL,
    ADD COLUMN scope ENUM('server','direct','group') NOT NULL DEFAULT 'server',
    ADD COLUMN dm_key VARCHAR(64) NULL,
    ADD UNIQUE KEY uk_channels_dm_key (dm_key);

-- 会话成员：只有 direct/group 使用；server 频道成员仍以 server_members 为准。
CREATE TABLE channel_members (
    channel_id BIGINT   NOT NULL,
    user_id    BIGINT   NOT NULL,
    role       ENUM('owner','member') NOT NULL DEFAULT 'member',
    joined_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (channel_id, user_id),
    KEY idx_channel_members_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
