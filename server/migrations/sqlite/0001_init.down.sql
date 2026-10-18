-- 0001_init.down.sql — SQLite 方言，逆序删表（schema 无 FOREIGN KEY 声明，删除顺序不敏感）
DROP TABLE IF EXISTS invites;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS server_members;
DROP TABLE IF EXISTS servers;
DROP TABLE IF EXISTS friendships;
DROP TABLE IF EXISTS users;
