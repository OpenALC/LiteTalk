-- 0002_guest.up.sql — 游客登录（simple 模式一键进入的临时身份标识）
ALTER TABLE users ADD COLUMN is_guest TINYINT(1) NOT NULL DEFAULT 0 AFTER email_verified;
