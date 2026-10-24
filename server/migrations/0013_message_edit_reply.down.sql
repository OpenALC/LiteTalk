-- 0013_message_edit_reply.down.sql — 顺逆序回滚（#39 B1）
DROP TABLE IF EXISTS message_mentions;
ALTER TABLE messages
    DROP INDEX idx_messages_reply_to,
    DROP COLUMN reply_to,
    DROP COLUMN deleted_at,
    DROP COLUMN edited_at;
