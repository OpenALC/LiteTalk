-- 0010_roles_permissions.down.sql — 只撤四张新表，不动旧表（server_members.role 保持原样）。
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS channel_overwrites;
DROP TABLE IF EXISTS member_roles;
DROP TABLE IF EXISTS roles;
