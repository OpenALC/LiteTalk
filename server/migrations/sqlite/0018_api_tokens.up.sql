-- 0018_api_tokens.up.sql — 个人访问令牌与机器人令牌（#51 #54；SQLite 方言）
-- 语义与 MySQL 版一致：ENUM → TEXT CHECK，唯一键独立 CREATE UNIQUE INDEX。
-- users.is_bot 就地加列，不重建表。
CREATE TABLE api_tokens (
    id           INTEGER      PRIMARY KEY,
    user_id      INTEGER      NOT NULL,
    name         VARCHAR(64)  NOT NULL,
    prefix       CHAR(8)      NOT NULL,
    secret_hash  CHAR(64)     NOT NULL,
    kind         TEXT         NOT NULL CHECK (kind IN ('pat','bot')),
    scopes       VARCHAR(255) NOT NULL,
    expires_at   DATETIME     NULL,
    last_used_at DATETIME     NULL,
    revoked_at   DATETIME     NULL,
    created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX uk_api_tokens_prefix ON api_tokens (prefix);
CREATE INDEX idx_api_tokens_user ON api_tokens (user_id);

ALTER TABLE users ADD COLUMN is_bot TINYINT(1) NOT NULL DEFAULT 0;
