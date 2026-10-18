-- 0001_init.up.sql — LiteTalk 核心表（SQLite 方言；与 MySQL 侧同号同名，version 一致）
-- 约定：id INTEGER PRIMARY KEY（rowid 天然自增，不写 AUTOINCREMENT）；ENUM → TEXT CHECK；
-- 索引独立 CREATE INDEX；不写 ENGINE/CHARSET/AFTER/触发器（splitSQL 按 ; 切分会碎）。
CREATE TABLE users (
    id            INTEGER      PRIMARY KEY,
    email         VARCHAR(255) NOT NULL,
    nickname      VARCHAR(16)  NOT NULL,
    tag           CHAR(4)      NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    email_verified TINYINT(1)  NOT NULL DEFAULT 0,
    status        TINYINT      NOT NULL DEFAULT 0,
    created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX uk_users_email ON users (email);
CREATE UNIQUE INDEX uk_users_nickname_tag ON users (nickname, tag);

CREATE TABLE friendships (
    id         INTEGER  PRIMARY KEY,
    user_id    INTEGER  NOT NULL,
    friend_id  INTEGER  NOT NULL,
    status     TEXT     NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','blocked')),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX uk_friendships_pair ON friendships (user_id, friend_id);
CREATE INDEX idx_friendships_friend ON friendships (friend_id);

CREATE TABLE servers (
    id         INTEGER      PRIMARY KEY,
    name       VARCHAR(64)  NOT NULL,
    owner_id   INTEGER      NOT NULL,
    icon_name  VARCHAR(64)  NOT NULL DEFAULT 'messages-square',
    icon_color VARCHAR(16)  NOT NULL DEFAULT '#4FA4F3',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_servers_owner ON servers (owner_id);

CREATE TABLE server_members (
    server_id INTEGER  NOT NULL,
    user_id   INTEGER  NOT NULL,
    role      TEXT     NOT NULL DEFAULT 'member' CHECK (role IN ('owner','admin','member')),
    joined_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (server_id, user_id)
);
CREATE INDEX idx_server_members_user ON server_members (user_id);

CREATE TABLE channels (
    id         INTEGER     PRIMARY KEY,
    server_id  INTEGER     NOT NULL,
    name       VARCHAR(64) NOT NULL,
    type       TEXT        NOT NULL DEFAULT 'text' CHECK (type IN ('text','voice')),
    position   INT         NOT NULL DEFAULT 0,
    created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_channels_server ON channels (server_id);

CREATE TABLE messages (
    id         INTEGER   PRIMARY KEY,
    channel_id INTEGER   NOT NULL,
    user_id    INTEGER   NOT NULL,
    content    TEXT      NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_messages_channel_id ON messages (channel_id, id);

CREATE TABLE invites (
    code       VARCHAR(32) NOT NULL,
    server_id  INTEGER     NOT NULL,
    creator_id INTEGER     NOT NULL,
    max_uses   INT         NOT NULL DEFAULT 0,
    uses       INT         NOT NULL DEFAULT 0,
    expires_at DATETIME    NULL,
    created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (code)
);
CREATE INDEX idx_invites_server ON invites (server_id);
