-- 0012_conversations.down.sql — 顺逆序回滚（#38 B1）
-- 先清非 server 频道及其消息/覆盖/成员，再恢复 NOT NULL：顺序反了会因
-- 现存 NULL server_id 直接 ALTER 失败。server_id IS NULL 一并兜住脏行。
DELETE FROM messages WHERE channel_id IN
    (SELECT id FROM channels WHERE scope <> 'server' OR server_id IS NULL);
DELETE FROM channel_overwrites WHERE channel_id IN
    (SELECT id FROM channels WHERE scope <> 'server' OR server_id IS NULL);
DELETE FROM channel_members WHERE channel_id IN
    (SELECT id FROM channels WHERE scope <> 'server' OR server_id IS NULL);
DELETE FROM channels WHERE scope <> 'server' OR server_id IS NULL;

ALTER TABLE channels
    DROP INDEX uk_channels_dm_key,
    DROP COLUMN dm_key,
    DROP COLUMN scope,
    MODIFY COLUMN server_id BIGINT NOT NULL;

DROP TABLE IF EXISTS channel_members;
