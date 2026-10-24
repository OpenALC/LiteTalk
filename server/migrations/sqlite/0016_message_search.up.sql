-- 0016_message_search.up.sql — 消息正文全文索引（#42 B1，SQLite 方言）
-- FTS5 外部内容表：索引不复制正文，查询时按 rowid 回表拿 messages.content。
-- trigram 分词器按 3 个 Unicode 字符滑窗，适合中文子串；不足 3 个字符的查询
-- 无法命中 trigram，由 store.SearchMessages 回退到 LIKE。
--
-- 这里不用 FTS5 官方触发器方案：迁移执行器 splitSQL 明确拒绝 CREATE TRIGGER
-- （BEGIN…END 体内的分号无法与语句分隔符区分，#75）。索引同步改在
-- store.SQLite 的消息写入路径（CreateMessage/UpdateMessageContent/
-- SoftDeleteMessage/批量删频道）同一事务内完成，写入与索引不会半途分叉。
-- 升级前已有的消息在本迁移里一次性回填。
CREATE VIRTUAL TABLE messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='id',
    tokenize='trigram'
);
INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages;
