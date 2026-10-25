-- 0012_conversations.down.sql — 清非 server 频道后按 0011 的形状重建（#38 B1；SQLite 方言）
-- 顺序：先删会话消息/覆盖/成员表，再重建 channels 恢复 server_id NOT NULL；
-- server_id IS NULL 的脏行一并清理，否则 INSERT ... SELECT 会撞 NOT NULL。
DELETE FROM messages WHERE channel_id IN
    (SELECT id FROM channels WHERE scope <> 'server' OR server_id IS NULL);
DELETE FROM channel_overwrites WHERE channel_id IN
    (SELECT id FROM channels WHERE scope <> 'server' OR server_id IS NULL);
DELETE FROM channels WHERE scope <> 'server' OR server_id IS NULL;
DROP TABLE IF EXISTS channel_members;

CREATE TABLE channels_old (
    id            INTEGER      PRIMARY KEY,
    server_id     INTEGER      NOT NULL,
    name          VARCHAR(64)  NOT NULL,
    type          TEXT         NOT NULL DEFAULT 'text' CHECK (type IN ('text','voice')),
    category_id   BIGINT       NULL,
    position      INT          NOT NULL DEFAULT 0,
    created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    password_hash VARCHAR(255) NOT NULL DEFAULT '',
    user_limit    INT          NOT NULL DEFAULT 0,
    parent_id     BIGINT       NULL
);
INSERT INTO channels_old (id, server_id, name, type, category_id, position, created_at,
                          password_hash, user_limit, parent_id)
SELECT id, server_id, name, type, category_id, position, created_at,
       password_hash, user_limit, parent_id
  FROM channels;
DROP TABLE channels;
ALTER TABLE channels_old RENAME TO channels;
CREATE INDEX idx_channels_server ON channels (server_id);
CREATE INDEX idx_channels_category ON channels (category_id);
CREATE INDEX idx_channels_parent ON channels (parent_id);
