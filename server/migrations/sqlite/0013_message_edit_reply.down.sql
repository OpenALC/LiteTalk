-- 0013_message_edit_reply.down.sql — 顺逆序回滚（#39 B1；SQLite 方言）
-- SQLite 3.35+ 才支持 DROP COLUMN；reply_to 上的索引必须先删，否则 DROP COLUMN 报错。
DROP TABLE IF EXISTS message_mentions;
DROP INDEX IF EXISTS idx_messages_reply_to;
ALTER TABLE messages DROP COLUMN reply_to;
ALTER TABLE messages DROP COLUMN deleted_at;
ALTER TABLE messages DROP COLUMN edited_at;
