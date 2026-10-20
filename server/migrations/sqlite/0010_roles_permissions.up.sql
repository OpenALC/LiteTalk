-- 0010_roles_permissions.up.sql — 角色与权限数据模型（#35 B1；SQLite 方言）
-- 整轮在事务内执行（Migrate 包事务），回填仍用 WHERE NOT EXISTS，与 MySQL 版语义一致。
-- 与 MySQL 差异：无 AUTO_INCREMENT/ON UPDATE/ENUM，updated_at 由应用层写。
CREATE TABLE roles (
    id          INTEGER     PRIMARY KEY,
    server_id   INTEGER     NOT NULL,
    name        VARCHAR(32) NOT NULL,
    color       VARCHAR(7)  NOT NULL DEFAULT '#4FA4F3',
    position    INT         NOT NULL DEFAULT 0,
    -- 只用低 15 位；有符号列存无符号位掩码，读写两侧都按 int64 走。
    permissions BIGINT      NOT NULL DEFAULT 0,
    -- 自定义角色为 NULL；SQLite 唯一索引把 NULL 彼此视为不同，允许多个自定义角色。
    builtin_key VARCHAR(16) NULL,
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX uk_roles_server_builtin ON roles (server_id, builtin_key);
CREATE INDEX idx_roles_server_position ON roles (server_id, position, id);

CREATE TABLE member_roles (
    server_id   INTEGER  NOT NULL,
    user_id     INTEGER  NOT NULL,
    role_id     INTEGER  NOT NULL,
    assigned_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    assigned_by INTEGER  NOT NULL DEFAULT 0,
    PRIMARY KEY (server_id, user_id, role_id)
);
CREATE INDEX idx_member_roles_role ON member_roles (role_id);
CREATE INDEX idx_member_roles_user ON member_roles (user_id);

CREATE TABLE channel_overwrites (
    channel_id  INTEGER  NOT NULL,
    target_type TEXT     NOT NULL CHECK (target_type IN ('role','member')),
    target_id   INTEGER  NOT NULL,
    allow       BIGINT   NOT NULL DEFAULT 0,
    deny        BIGINT   NOT NULL DEFAULT 0,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (channel_id, target_type, target_id)
);
CREATE INDEX idx_channel_overwrites_target ON channel_overwrites (target_type, target_id);

CREATE TABLE audit_log (
    id          INTEGER     NOT NULL PRIMARY KEY,
    server_id   BIGINT      NOT NULL,
    actor_id    BIGINT      NOT NULL DEFAULT 0,
    action      VARCHAR(64) NOT NULL,
    target_type VARCHAR(16) NULL,
    target_id   BIGINT      NULL,
    channel_id  BIGINT      NULL,
    detail      TEXT        NOT NULL,
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_audit_log_server ON audit_log (server_id, id);
CREATE INDEX idx_audit_log_actor ON audit_log (actor_id, id);
CREATE INDEX idx_audit_log_action ON audit_log (action, id);

-- 默认角色回填：每个已有 server 三条 builtin（值与 store.BuiltinRoleTemplates 同源）。
INSERT INTO roles (server_id, name, position, permissions, builtin_key)
SELECT s.id, '@everyone', 0, 8207, 'member'
  FROM servers s
 WHERE NOT EXISTS (
     SELECT 1 FROM roles r WHERE r.server_id = s.id AND r.builtin_key = 'member'
 );

INSERT INTO roles (server_id, name, position, permissions, builtin_key)
SELECT s.id, '管理员', 1, 32767, 'admin'
  FROM servers s
 WHERE NOT EXISTS (
     SELECT 1 FROM roles r WHERE r.server_id = s.id AND r.builtin_key = 'admin'
 );

INSERT INTO roles (server_id, name, position, permissions, builtin_key)
SELECT s.id, '服主', 2, 32767, 'owner'
  FROM servers s
 WHERE NOT EXISTS (
     SELECT 1 FROM roles r WHERE r.server_id = s.id AND r.builtin_key = 'owner'
 );

-- 每个已有成员分配 @everyone；assigned_at 取 joined_at（入会即持有）。
INSERT INTO member_roles (server_id, user_id, role_id, assigned_at, assigned_by)
SELECT m.server_id, m.user_id, r.id, m.joined_at, 0
  FROM server_members m
  JOIN roles r ON r.server_id = m.server_id AND r.builtin_key = 'member'
 WHERE NOT EXISTS (
     SELECT 1 FROM member_roles mr
      WHERE mr.server_id = m.server_id AND mr.user_id = m.user_id AND mr.role_id = r.id
 );

-- legacy server_members.role 的 admin/owner 再补对应 builtin；不动 legacy 列本身。
INSERT INTO member_roles (server_id, user_id, role_id, assigned_at, assigned_by)
SELECT m.server_id, m.user_id, r.id, m.joined_at, 0
  FROM server_members m
  JOIN roles r ON r.server_id = m.server_id
              AND ((m.role = 'admin' AND r.builtin_key = 'admin')
                OR (m.role = 'owner' AND r.builtin_key = 'owner'))
 WHERE NOT EXISTS (
     SELECT 1 FROM member_roles mr
      WHERE mr.server_id = m.server_id AND mr.user_id = m.user_id AND mr.role_id = r.id
 );
