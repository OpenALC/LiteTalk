-- 0008_friendship_pair.up.sql — 好友关系按无序对唯一（#76）
-- 旧唯一键只约束 (user_id, friend_id) 方向：A→B 与 B→A 各存一行时好友列表重复。
-- 先删除反向重复（同一无序对保留最早一行，id 由 Snowflake 生成、与创建时间同序），
-- 再建无序对唯一索引。函数索引要求 MySQL 8.0.13+；docker-compose 固定 mysql:8.4。
DELETE f1 FROM friendships AS f1
INNER JOIN friendships AS f2
    ON f1.user_id = f2.friend_id
   AND f1.friend_id = f2.user_id
   AND f1.id > f2.id;

CREATE UNIQUE INDEX uk_friendships_unordered
    ON friendships ((LEAST(user_id, friend_id)), (GREATEST(user_id, friend_id)));
