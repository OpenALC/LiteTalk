-- 0022_webhook_creator.up.sql — 出站订阅记录创建者（#52，PR118 P1-5）
-- 出站订阅此前不记创建者，投递无法按创建者频道可见性收窄：ManageServer 用户
-- 可订阅自己看不到的私有频道并收到全部消息内容。新增 creator_id（创建订阅的
-- 主体用户 id，仅创建时写入、之后不可变），投递前按创建者当前 ViewChannel 过滤。
--
-- 旧行语义：creator_id 为 NULL 视为「由服务器创建者（owner）代持」。up 里用关联
-- 子查询把既有行回填为所属服务器的 owner_id——v1.9.0–v1.9.5 期间创建订阅要求
-- ManageServer（通常即服主/管理员本人），按 owner 判定与当时的真实创建者最接近。
-- 服务器已删除的孤儿行子查询落空保持 NULL，由投递侧 fail-closed 跳过。
ALTER TABLE webhooks ADD COLUMN creator_id BIGINT NULL;
UPDATE webhooks
   SET creator_id = (SELECT owner_id FROM servers WHERE servers.id = webhooks.server_id)
 WHERE creator_id IS NULL;
