-- 0008_friendship_pair.up.sql — 好友关系按无序对唯一（SQLite 方言，#76，#100 修正去重规则）
-- 旧唯一索引只约束 (user_id, friend_id) 方向：A→B 与 B→A 各存一行时好友列表重复。
-- 先按无序对去重，只保留一行，再建无序对唯一索引。
--
-- #100 修正：原规则只保留 MIN(id)，若反向 accepted 的 id 较大，会被较小的 pending
-- 顶掉，已接受的好友关系因此丢失。现规则为状态优先 accepted > blocked > pending，
-- 同状态保留最小 id（id 由 Snowflake 生成、与创建时间同序）。
-- 这是一次性例外：0008 已发布的去重规则有缺陷，经用户批准就地修正。0008 已记录进
-- schema_migrations 的实例不会重跑本文件，也不会回滚旧规则删掉的行；受影响的用户
-- 重新发起并接受好友即可恢复。修正只影响尚未执行 0008 的升级或新装实例。
DELETE FROM friendships
 WHERE EXISTS (
     SELECT 1 FROM friendships AS f2
      WHERE f2.user_id = friendships.friend_id
        AND f2.friend_id = friendships.user_id
        AND (
             (CASE f2.status WHEN 'accepted' THEN 0 WHEN 'blocked' THEN 1 ELSE 2 END)
             < (CASE friendships.status WHEN 'accepted' THEN 0 WHEN 'blocked' THEN 1 ELSE 2 END)
          OR (
             (CASE f2.status WHEN 'accepted' THEN 0 WHEN 'blocked' THEN 1 ELSE 2 END)
             = (CASE friendships.status WHEN 'accepted' THEN 0 WHEN 'blocked' THEN 1 ELSE 2 END)
             AND f2.id < friendships.id
          )
        )
 );

CREATE UNIQUE INDEX uk_friendships_unordered
    ON friendships (MIN(user_id, friend_id), MAX(user_id, friend_id));
