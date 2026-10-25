-- 0015_attachments.up.sql — 消息附件元数据（#41 B1）
-- 二进制内容存 blob（local 目录或 S3），本表只放元数据；storage_key/thumb_key 是
-- blob 内的不透明键，由附件 ID 拼接（见 internal/attachment.Keys），不含原始文件名。
-- message_id 单归属：NULL 表示已上传但尚未被消息引用；消息落库事务内认领
-- （UPDATE ... WHERE uploader_id = ? AND message_id IS NULL），一条附件只能挂一条消息。
-- access_key 是 32 字节随机数的 hex，作为下载能力凭证（不可枚举）；sha256 供去重/校验。
-- 软删消息的附件行仍保留，是否允许下载由 API 按消息状态校验，不在本层级联删除。
-- id 由调用方预置 Snowflake，不用 AUTO_INCREMENT；access_key/sha256 用 ascii_bin，
-- 凭证按字节精确比对，不受默认 utf8mb4_unicode_ci 的大小写不敏感影响。
CREATE TABLE attachments (
    id           BIGINT       NOT NULL,
    uploader_id  BIGINT       NOT NULL,
    message_id   BIGINT       NULL,
    filename     VARCHAR(255) NOT NULL,
    content_type VARCHAR(128) NOT NULL,
    size         BIGINT       NOT NULL,
    width        INT          NOT NULL DEFAULT 0,
    height       INT          NOT NULL DEFAULT 0,
    storage_key  VARCHAR(128) NOT NULL,
    thumb_key    VARCHAR(128) NOT NULL DEFAULT '',
    access_key   CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sha256       CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_attachments_access (access_key),
    KEY idx_attachments_message (message_id),
    KEY idx_attachments_uploader (uploader_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
