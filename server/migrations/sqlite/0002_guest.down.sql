-- 0002_guest.down.sql — 回滚：移除游客标识列（游客行保留，视为普通用户）
ALTER TABLE users DROP COLUMN is_guest;
