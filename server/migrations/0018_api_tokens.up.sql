-- 0018_api_tokens.up.sql — 个人访问令牌与机器人令牌（#51 #54）
-- 一张表两种 kind：pat 挂普通用户（POST /api/v1/tokens 创建），bot 挂 is_bot 用户
-- （创建入口属 #51）。secret_hash 是 SHA-256(secret) 的十六进制——令牌是高熵随机串，
-- 校验按 prefix 普通索引定位一行后常数时间比对；bcrypt 无法按前缀建索引、
-- 每次校验约 200ms 会拖垮所有请求（详见 internal/token 注释）。
CREATE TABLE api_tokens (
    id           BIGINT      NOT NULL AUTO_INCREMENT,
    user_id      BIGINT      NOT NULL,
    name         VARCHAR(64) NOT NULL,
    prefix       CHAR(8)     NOT NULL,
    secret_hash  CHAR(64)    NOT NULL,
    kind         ENUM('pat','bot') NOT NULL,
    scopes       VARCHAR(255) NOT NULL,
    expires_at   DATETIME    NULL,
    last_used_at DATETIME    NULL,
    revoked_at   DATETIME    NULL,
    created_at   DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_api_tokens_prefix (prefix),
    KEY idx_api_tokens_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- bot 账号复用 users 表（#51）：is_bot=1 的行 password_hash 落空串（无法密码登录）、
-- email 用 <prefix>@bots.invalid，不进好友候选/@提及候选/presence 广播。
ALTER TABLE users ADD COLUMN is_bot TINYINT(1) NOT NULL DEFAULT 0;
