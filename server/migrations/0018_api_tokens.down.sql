-- 0018_api_tokens.down.sql — 逆序回滚（#51 #54）
ALTER TABLE users DROP COLUMN is_bot;
DROP TABLE IF EXISTS api_tokens;
