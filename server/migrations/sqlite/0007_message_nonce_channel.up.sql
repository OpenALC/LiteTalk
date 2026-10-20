-- 0007_message_nonce_channel.up.sql — nonce 去重键加入频道维度（SQLite 方言，#31）
-- 跨频道重放同一个 nonce 不能命中另一个频道的消息：(channel_id, user_id, nonce)。
DROP INDEX idx_messages_user_nonce;
CREATE UNIQUE INDEX idx_messages_channel_user_nonce ON messages (channel_id, user_id, nonce);
