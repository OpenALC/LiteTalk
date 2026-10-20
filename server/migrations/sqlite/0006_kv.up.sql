-- 0006_kv.up.sql — 持久化 KV（SQLite 方言，#83）
-- expires_at 为 UTC DATETIME 文本（DSN 的 _time_format=datetime&_timezone=UTC），
-- NULL = 永不过期。SQLite 默认 BINARY 排序，键与 MySQL 的 utf8mb4_bin 同语义。
CREATE TABLE kv (
    kv_key     VARCHAR(512) NOT NULL PRIMARY KEY,
    kv_value   TEXT NOT NULL,
    expires_at DATETIME NULL
);
