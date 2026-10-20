-- 0008_friendship_pair.down.sql — 逆序回滚（去重删除不可逆，只撤唯一索引）
DROP INDEX uk_friendships_unordered ON friendships;
