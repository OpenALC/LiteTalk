-- 0013_message_edit_reply.up.sql — 消息编辑/软删除/回复/@提及（#39 B1；SQLite 方言）
-- SQLite 不支持多子句 ALTER，逐列 ADD COLUMN；新列可空无默认值，旧行读回 NULL。
ALTER TABLE messages ADD COLUMN edited_at DATETIME NULL;
ALTER TABLE messages ADD COLUMN deleted_at DATETIME NULL;
ALTER TABLE messages ADD COLUMN reply_to BIGINT NULL;
CREATE INDEX idx_messages_reply_to ON messages (reply_to);

CREATE TABLE message_mentions (
    message_id INTEGER NOT NULL,
    user_id    INTEGER NOT NULL,
    PRIMARY KEY (message_id, user_id)
);
CREATE INDEX idx_message_mentions_user ON message_mentions (user_id, message_id);
