-- 0002_guest.up.sql — 游客登录（SQLite 方言；AFTER 子句不支持，列序对按列名查询无影响）
ALTER TABLE users ADD COLUMN is_guest TINYINT(1) NOT NULL DEFAULT 0;
