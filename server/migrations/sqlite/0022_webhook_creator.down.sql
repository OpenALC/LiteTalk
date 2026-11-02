-- 0022_webhook_creator.down.sql — 移除 creator_id（#52，PR118 P1-5）
-- 与 up 成对：回滚只删列，不影响订阅的 CRUD 与投递记录。
ALTER TABLE webhooks DROP COLUMN creator_id;
