-- 0015_attachments.up.sql — 消息附件元数据（#41 B1；SQLite 方言）
-- 二进制内容存 blob（local 目录或 S3），本表只放元数据；storage_key/thumb_key 是
-- blob 内的不透明键，由附件 ID 拼接（见 internal/attachment.Keys），不含原始文件名。
-- message_id 单归属：NULL 表示已上传但尚未被消息引用；消息落库事务内认领
-- （UPDATE ... WHERE uploader_id = ? AND message_id IS NULL），一条附件只能挂一条消息。
-- access_key 是 32 字节随机数的 hex，作为下载能力凭证（不可枚举）；sha256 供去重/校验。
-- 软删消息的附件行仍保留，是否允许下载由 API 按消息状态校验，不在本层级联删除。
-- SQLite 的 TEXT 默认按字节比较（BINARY 排序规则），凭证比对与 MySQL ascii_bin 同口径。
CREATE TABLE attachments (
    id           INTEGER  PRIMARY KEY,
    uploader_id  INTEGER  NOT NULL,
    message_id   INTEGER  NULL,
    filename     TEXT     NOT NULL,
    content_type TEXT     NOT NULL,
    size         INTEGER  NOT NULL,
    width        INTEGER  NOT NULL DEFAULT 0,
    height       INTEGER  NOT NULL DEFAULT 0,
    storage_key  TEXT     NOT NULL,
    thumb_key    TEXT     NOT NULL DEFAULT '',
    access_key   TEXT     NOT NULL,
    sha256       TEXT     NOT NULL,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX uk_attachments_access ON attachments (access_key);
CREATE INDEX idx_attachments_message ON attachments (message_id);
CREATE INDEX idx_attachments_uploader ON attachments (uploader_id);
