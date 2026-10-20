-- 0008_friendship_pair.up.sql — 好友关系按无序对唯一（SQLite 方言，#76）
-- 旧唯一索引只约束 (user_id, friend_id) 方向：A→B 与 B→A 各存一行时好友列表重复。
-- 先按无序对归组，只保留 MIN(id)（最早一行），再建无序对唯一索引。
DELETE FROM friendships
 WHERE id NOT IN (
     SELECT MIN(id) FROM friendships
      GROUP BY MIN(user_id, friend_id), MAX(user_id, friend_id)
 );

CREATE UNIQUE INDEX uk_friendships_unordered
    ON friendships (MIN(user_id, friend_id), MAX(user_id, friend_id));
