-- 0006_kv.up.sql — 持久化 KV（#83）
-- 未配置 Redis 时刷新令牌/游客身份/验证码/限流状态落库，重启不丢。
-- kv_key 用二进制排序规则：Redis 键是精确字节匹配，ci 排序会把大小写不同的键合并。
CREATE TABLE kv (
    kv_key     VARCHAR(512) COLLATE utf8mb4_bin NOT NULL,
    kv_value   TEXT NOT NULL,
    expires_at DATETIME NULL,
    PRIMARY KEY (kv_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
