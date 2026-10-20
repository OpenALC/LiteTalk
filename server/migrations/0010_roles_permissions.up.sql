-- 0010_roles_permissions.up.sql — 角色与权限数据模型（#35 B1）
-- roles/member_roles/channel_overwrites/audit_log + 默认角色回填。
-- MySQL DDL 隐式提交、本文件无事务：建表与回填必须可重入（IF NOT EXISTS /
-- WHERE NOT EXISTS），中途失败后 schema_migrations 未记账，重启整文件重跑不重复写。
CREATE TABLE IF NOT EXISTS roles (
    id          BIGINT      NOT NULL AUTO_INCREMENT,
    server_id   BIGINT      NOT NULL,
    name        VARCHAR(32) NOT NULL,
    color       VARCHAR(7)  NOT NULL DEFAULT '#4FA4F3',
    position    INT         NOT NULL DEFAULT 0,
    -- 只用低 15 位；有符号列存无符号位掩码，读写两侧都按 int64 走。
    permissions BIGINT      NOT NULL DEFAULT 0,
    -- 三值 member/admin/owner 按字节比对，不能用默认 unicode_ci（大小写变体会撞唯一键）；
    -- 自定义角色为 NULL，唯一索引允许多个 NULL。
    builtin_key VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_roles_server_builtin (server_id, builtin_key),
    KEY idx_roles_server_position (server_id, position, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS member_roles (
    server_id   BIGINT   NOT NULL,
    user_id     BIGINT   NOT NULL,
    role_id     BIGINT   NOT NULL,
    assigned_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    assigned_by BIGINT   NOT NULL DEFAULT 0,
    PRIMARY KEY (server_id, user_id, role_id),
    KEY idx_member_roles_role (role_id),
    KEY idx_member_roles_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS channel_overwrites (
    channel_id  BIGINT           NOT NULL,
    target_type ENUM('role','member') NOT NULL,
    target_id   BIGINT           NOT NULL,
    allow       BIGINT           NOT NULL DEFAULT 0,
    deny        BIGINT           NOT NULL DEFAULT 0,
    updated_at  DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (channel_id, target_type, target_id),
    KEY idx_channel_overwrites_target (target_type, target_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGINT      NOT NULL,
    server_id   BIGINT      NOT NULL,
    actor_id    BIGINT      NOT NULL DEFAULT 0,
    action      VARCHAR(64) NOT NULL,
    target_type VARCHAR(16) NULL,
    target_id   BIGINT      NULL,
    channel_id  BIGINT      NULL,
    detail      TEXT        NOT NULL,
    created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_audit_log_server (server_id, id),
    KEY idx_audit_log_actor (actor_id, id),
    KEY idx_audit_log_action (action, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 默认角色回填：每个已有 server 三条 builtin（值与 store.BuiltinRoleTemplates 同源）。
-- 回填行的 id 交给 AUTO_INCREMENT（运行时 CreateRole 才用 Snowflake）。
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

-- 每个已有成员分配 @everyone；assigned_at 取 joined_at（入会即持有，不留时间假象）。
INSERT INTO member_roles (server_id, user_id, role_id, assigned_at, assigned_by)
SELECT m.server_id, m.user_id, r.id, m.joined_at, 0
  FROM server_members m
  JOIN roles r ON r.server_id = m.server_id AND r.builtin_key = 'member'
 WHERE NOT EXISTS (
     SELECT 1 FROM member_roles mr
      WHERE mr.server_id = m.server_id AND mr.user_id = m.user_id AND mr.role_id = r.id
 );

-- legacy server_members.role 的 admin/owner 再补对应 builtin；不动 legacy 列本身
-- （投影同步由 SetMemberRoles 的应用层逻辑负责）。
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
