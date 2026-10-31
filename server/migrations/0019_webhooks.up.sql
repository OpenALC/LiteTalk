-- 0019_webhooks.up.sql — 入站与出站 Webhook（#52）
-- 出站 webhooks：服务器级事件订阅，投递用 HMAC-SHA256 签名（密钥即 secret）；
-- 连续失败达熔断阈值由应用层置 disabled=1 并写 disabled_reason，手动重新启用清零计数。
-- 入站 inbound_webhooks：频道级投递入口，secret 参与 URL（/api/v1/webhooks/{id}/{secret}），
-- 泄露即作废（重建即换），旧 secret 校验失败一律按不存在处理。
-- webhook_deliveries：投递记录，应用层按保留期（默认 7 天）定期清理，
-- idx_webhook_deliveries_created 服务清理扫描。
CREATE TABLE webhooks (
    id              BIGINT       NOT NULL AUTO_INCREMENT,
    server_id       BIGINT       NOT NULL,
    url             VARCHAR(2048) NOT NULL,
    secret          CHAR(64)     NOT NULL,
    events          VARCHAR(255) NOT NULL,
    disabled        TINYINT(1)   NOT NULL DEFAULT 0,
    disabled_reason VARCHAR(64)  NOT NULL DEFAULT '',
    fail_count      INT          NOT NULL DEFAULT 0,
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_webhooks_server (server_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE inbound_webhooks (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    channel_id  BIGINT       NOT NULL,
    secret      CHAR(64)     NOT NULL,
    description VARCHAR(255) NOT NULL DEFAULT '',
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_inbound_webhooks_channel (channel_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE webhook_deliveries (
    id          BIGINT      NOT NULL AUTO_INCREMENT,
    webhook_id  BIGINT      NOT NULL,
    event       VARCHAR(64) NOT NULL,
    attempts    INT         NOT NULL DEFAULT 0,
    ok          TINYINT(1)  NOT NULL DEFAULT 0,
    status_code INT         NOT NULL DEFAULT 0,
    error       VARCHAR(255) NOT NULL DEFAULT '',
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_webhook_deliveries_hook (webhook_id),
    KEY idx_webhook_deliveries_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
