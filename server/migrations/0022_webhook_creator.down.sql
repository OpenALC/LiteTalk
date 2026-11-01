-- 0022_webhook_creator.down.sql — 移除 creator_id（#52，PR118 P1-5）
-- 回滚后旧行为复归：出站投递不再按创建者可见性收窄（该列只被投递过滤与创建
-- 写入使用，回滚不影响订阅的 CRUD 与投递记录）。
ALTER TABLE webhooks DROP COLUMN creator_id;
