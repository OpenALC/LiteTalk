-- 0020_push_devices.up.sql — 移动推送设备注册表（#53）
-- push_token 是生产凭据的等价物：明文落库仅供推送通道投递使用，任何接口不回显、
-- 日志一律打码（只留前 8 位）。幂等键不用 push_token 本列——FCM/APNs 的 token
-- 可达 4KB，超出唯一索引长度上限，(user_id, token_hash) 用 SHA-256 定长十六进制。
CREATE TABLE push_devices (
    id           BIGINT      NOT NULL,
    user_id      BIGINT      NOT NULL,
    platform     ENUM('ios','android') NOT NULL,
    push_token   TEXT        NOT NULL,
    token_hash   CHAR(64)    NOT NULL,
    last_seen_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    created_at   DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_push_devices_user_token (user_id, token_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
