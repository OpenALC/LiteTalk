-- 0007_message_nonce_channel.down.sql — 逆序回滚
-- 注意：若已存在跨频道同 nonce 的数据，旧唯一键会拒绝重建，需先人工清理。
DROP INDEX idx_messages_channel_user_nonce ON messages;
CREATE UNIQUE INDEX idx_messages_user_nonce ON messages (user_id, nonce);
