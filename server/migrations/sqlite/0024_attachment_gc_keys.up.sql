-- #105 无行对象回收逐键核对引用，避免对附件表反复全表扫描。
CREATE INDEX idx_attachments_storage_key ON attachments (storage_key);
CREATE INDEX idx_attachments_thumb_key ON attachments (thumb_key);
