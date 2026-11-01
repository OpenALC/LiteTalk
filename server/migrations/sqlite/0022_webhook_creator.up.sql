-- 0022_webhook_creator.up.sql — 出站订阅记录创建者（#52，PR118 P1-5）
-- 语义与 MySQL 同号迁移一致：新增 creator_id 并把既有行回填为所属服务器
-- owner_id（关联子查询回填，SQLite 同形态支持）；服务器已删的孤儿行保持 NULL，
-- 由投递侧 fail-closed 跳过。
ALTER TABLE webhooks ADD COLUMN creator_id INTEGER NULL;
UPDATE webhooks
   SET creator_id = (SELECT owner_id FROM servers WHERE servers.id = webhooks.server_id)
 WHERE creator_id IS NULL;
