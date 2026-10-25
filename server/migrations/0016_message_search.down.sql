-- 0016_message_search.down.sql — 删除消息全文索引（#42 B1，MySQL 方言）
ALTER TABLE messages DROP INDEX ft_messages_content;
