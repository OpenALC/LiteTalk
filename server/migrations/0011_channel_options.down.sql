-- 0011_channel_options.down.sql — 逆序删列；先删索引（列被索引引用时 DROP COLUMN 受阻）。
ALTER TABLE channels
    DROP INDEX idx_channels_parent,
    DROP COLUMN parent_id,
    DROP COLUMN user_limit,
    DROP COLUMN password_hash;
