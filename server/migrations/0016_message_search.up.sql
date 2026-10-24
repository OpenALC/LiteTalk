-- 0016_message_search.up.sql — 消息正文全文索引（#42 B1，MySQL 方言）
-- 中文没有空格分词，用 MySQL 内置 ngram 解析器按 2-gram 切分（ngram_token_size
-- 默认 2）：查询词不足 2 个字符时 ngram 无法命中，由 store.SearchMessages 回退
-- 到 LIKE。只索引 content；软删会把正文清空，索引项随之自然失效，无需额外维护。
-- 生产为 MySQL 8.0/8.4；ngram 是内置 parser，不依赖任何插件。
ALTER TABLE messages ADD FULLTEXT KEY ft_messages_content (content) WITH PARSER ngram;
