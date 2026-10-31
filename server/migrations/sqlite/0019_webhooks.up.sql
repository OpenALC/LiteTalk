-- 0019_webhooks.up.sql — 入站与出站 Webhook（#52；SQLite 方言）
-- 语义与 MySQL 版一致：无 ENUM 可转换（本表无枚举列），AUTO_INCREMENT → INTEGER PRIMARY KEY，
-- 唯一索引独立 CREATE INDEX（本表无唯一键，只有普通索引）。
CREATE TABLE webhooks (
    id              INTEGER       PRIMARY KEY,
    server_id       BIGINT        NOT NULL,
    url             VARCHAR(2048) NOT NULL,
    secret          CHAR(64)      NOT NULL,
    events          VARCHAR(255)  NOT NULL,
    disabled        TINYINT(1)    NOT NULL DEFAULT 0,
    disabled_reason VARCHAR(64)   NOT NULL DEFAULT '',
    fail_count      INT           NOT NULL DEFAULT 0,
    created_at      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_webhooks_server ON webhooks (server_id);

CREATE TABLE inbound_webhooks (
    id          INTEGER      PRIMARY KEY,
    channel_id  BIGINT       NOT NULL,
    secret      CHAR(64)     NOT NULL,
    description VARCHAR(255) NOT NULL DEFAULT '',
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_inbound_webhooks_channel ON inbound_webhooks (channel_id);

CREATE TABLE webhook_deliveries (
    id          INTEGER      PRIMARY KEY,
    webhook_id  BIGINT       NOT NULL,
    event       VARCHAR(64)  NOT NULL,
    attempts    INT          NOT NULL DEFAULT 0,
    ok          TINYINT(1)   NOT NULL DEFAULT 0,
    status_code INT          NOT NULL DEFAULT 0,
    error       VARCHAR(255) NOT NULL DEFAULT '',
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_webhook_deliveries_hook ON webhook_deliveries (webhook_id);
CREATE INDEX idx_webhook_deliveries_created ON webhook_deliveries (created_at);
