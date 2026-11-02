-- 0021_oidc_identities.up.sql — OIDC 外部身份绑定（#54）
-- (issuer, sub) 唯一：同一外部身份只允许绑定一个本地账号。自动绑定只发生在
-- 首次登录且 IdP 声明 email_verified 的场合（防账号劫持，绝不自动合并账号）。
-- email 记录绑定/建号时刻 IdP 声明的邮箱，仅审计用途，任何 API 不回显。
CREATE TABLE oidc_identities (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    issuer     VARCHAR(255) NOT NULL,
    sub        VARCHAR(255) NOT NULL,
    user_id    BIGINT       NOT NULL,
    email      VARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oidc_identities_issuer_sub (issuer, sub),
    KEY idx_oidc_identities_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
