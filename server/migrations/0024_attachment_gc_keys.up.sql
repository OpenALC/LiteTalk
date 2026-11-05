-- #105 两个键索引同一 DDL 添加，避免 MySQL 迁移逐条执行时半应用。
ALTER TABLE attachments
    ADD INDEX idx_attachments_storage_key (storage_key),
    ADD INDEX idx_attachments_thumb_key (thumb_key);
