-- 0020_push_devices.up.sql — 移动推送设备注册表（#53；SQLite 方言）
-- SQLite 无 ENUM：platform 用 TEXT + CHECK（与 MySQL 方言同口径 ios/android）。
-- last_seen_at/created_at 由 Store 写入时显式赋值（SQLite 没有 ON UPDATE）。
CREATE TABLE push_devices (
    id           INTEGER     NOT NULL,
    user_id      INTEGER     NOT NULL,
    platform     TEXT        NOT NULL CHECK (platform IN ('ios','android')),
    push_token   TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL,
    last_seen_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at   DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
);
CREATE UNIQUE INDEX uk_push_devices_user_token ON push_devices (user_id, token_hash);
