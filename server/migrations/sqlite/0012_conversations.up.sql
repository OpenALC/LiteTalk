-- 0012_conversations.up.sql — 私聊/群私聊（#38 B1；SQLite 方言）
-- SQLite 不能就地解除 NOT NULL：按 0001/0003/0011 的最终列定义重建 channels，
-- 旧行原样搬过去并补 scope='server'、dm_key=NULL。schema 没有声明外键，
-- DROP/RENAME 不会连带删除 messages/channel_overwrites 等表的数据。
CREATE TABLE channels_new (
    id            INTEGER      PRIMARY KEY,
    server_id     INTEGER      NULL,
    name          VARCHAR(64)  NOT NULL,
    type          TEXT         NOT NULL DEFAULT 'text' CHECK (type IN ('text','voice')),
    category_id   BIGINT       NULL,
    position      INT          NOT NULL DEFAULT 0,
    created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    password_hash VARCHAR(255) NOT NULL DEFAULT '',
    user_limit    INT          NOT NULL DEFAULT 0,
    parent_id     BIGINT       NULL,
    scope         TEXT         NOT NULL DEFAULT 'server' CHECK (scope IN ('server','direct','group')),
    dm_key        VARCHAR(64)  NULL
);
INSERT INTO channels_new (id, server_id, name, type, category_id, position, created_at,
                          password_hash, user_limit, parent_id, scope, dm_key)
SELECT id, server_id, name, type, category_id, position, created_at,
       password_hash, user_limit, parent_id, 'server', NULL
  FROM channels;
DROP TABLE channels;
ALTER TABLE channels_new RENAME TO channels;
CREATE INDEX idx_channels_server ON channels (server_id);
CREATE INDEX idx_channels_category ON channels (category_id);
CREATE INDEX idx_channels_parent ON channels (parent_id);
CREATE UNIQUE INDEX uk_channels_dm_key ON channels (dm_key);

-- 会话成员：只有 direct/group 使用；server 频道成员仍以 server_members 为准。
CREATE TABLE channel_members (
    channel_id INTEGER  NOT NULL,
    user_id    INTEGER  NOT NULL,
    role       TEXT     NOT NULL DEFAULT 'member' CHECK (role IN ('owner','member')),
    joined_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (channel_id, user_id)
);
CREATE INDEX idx_channel_members_user ON channel_members (user_id);
