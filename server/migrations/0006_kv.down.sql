-- 0006_kv.down.sql — 回滚：删除持久化 KV 表（重启后会话/验证码/限流随之丢失）
DROP TABLE IF EXISTS kv;
