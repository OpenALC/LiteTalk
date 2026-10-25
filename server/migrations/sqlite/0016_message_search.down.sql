-- 0016_message_search.down.sql — 删除消息全文索引（#42 B1，SQLite 方言）
DROP TABLE IF EXISTS messages_fts;
