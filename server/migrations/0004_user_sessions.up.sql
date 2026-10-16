-- 0004_user_sessions.up.sql — 多设备会话表（v1.1.0 批次 2）
-- 每设备一个会话：登录/注册/游客成功时 upsert（覆盖即吊销旧会话）。
-- sid 是 32 hex 随机串（由服务端生成），写入时由 auth.SessionIndex 缓存/回填。
CREATE TABLE user_sessions (
    user_id     BIGINT      NOT NULL,
    device_type VARCHAR(16) NOT NULL DEFAULT 'desktop',
    sid         CHAR(32)    NOT NULL,
    ip          VARCHAR(45) NOT NULL DEFAULT '',
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, device_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
