-- 0005_message_nonce.up.sql — 消息幂等 nonce（SQLite 方言，#31）
-- 发消息带客户端 nonce，(user_id, nonce) 唯一索引去重；重复插入回读原消息。
-- NULL nonce 不受唯一约束（SQLite 唯一索引里 NULL 互不相等）。
ALTER TABLE messages ADD COLUMN nonce VARCHAR(64) NULL;
CREATE UNIQUE INDEX idx_messages_user_nonce ON messages (user_id, nonce);
