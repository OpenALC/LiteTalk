-- 0021_oidc_identities.up.sql — OIDC 外部身份绑定（#54；SQLite 方言）
-- 与 MySQL 方言同口径：id 由 Store 显式赋值（Snowflake），created_at 写入时显式赋值。
CREATE TABLE oidc_identities (
    id         INTEGER      NOT NULL,
    issuer     TEXT         NOT NULL,
    sub        TEXT         NOT NULL,
    user_id    INTEGER      NOT NULL,
    email      TEXT         NOT NULL DEFAULT '',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
);
CREATE UNIQUE INDEX uk_oidc_identities_issuer_sub ON oidc_identities (issuer, sub);
CREATE INDEX idx_oidc_identities_user ON oidc_identities (user_id);
