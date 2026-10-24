-- 0013_message_edit_reply.up.sql — 消息编辑/软删除/回复/@提及（#39 B1）
-- deleted_at 是软删标记：行保留供回复引用，正文由 Store 清空。
-- reply_to 指向同表消息 id；不建外键——被引用消息跨频道存在、且删除后仍需回读。
-- 多子句 ALTER 是原子 DDL，避免「加了一半断电」的半应用状态。
ALTER TABLE messages
    ADD COLUMN edited_at DATETIME NULL,
    ADD COLUMN deleted_at DATETIME NULL,
    ADD COLUMN reply_to BIGINT NULL,
    ADD KEY idx_messages_reply_to (reply_to);

-- @提及：复合主键天然去重；user_id 前缀索引供「我被提及的消息」反查。
CREATE TABLE message_mentions (
    message_id BIGINT NOT NULL,
    user_id    BIGINT NOT NULL,
    PRIMARY KEY (message_id, user_id),
    KEY idx_message_mentions_user (user_id, message_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
