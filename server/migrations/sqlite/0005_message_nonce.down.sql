-- 0005_message_nonce.down.sql — 逆序回滚
DROP INDEX idx_messages_user_nonce;
ALTER TABLE messages DROP COLUMN nonce;
