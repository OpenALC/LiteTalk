-- 0004_user_sessions.up.sql — 多设备会话表（SQLite 方言）
-- 每设备一个会话：登录/注册/游客成功时 upsert（覆盖即吊销旧会话）。
-- updated_at 无 ON UPDATE CURRENT_TIMESTAMP，由 UpsertUserSession 的 DO UPDATE 显式写。
CREATE TABLE user_sessions (
    user_id     INTEGER     NOT NULL,
    device_type VARCHAR(16) NOT NULL DEFAULT 'desktop',
    sid         CHAR(32)    NOT NULL,
    ip          VARCHAR(45) NOT NULL DEFAULT '',
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, device_type)
);
