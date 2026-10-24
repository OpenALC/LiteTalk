-- 0014_read_states.up.sql — 每用户每频道的已读水位（#40 B1）
-- 水位 = 最后已读消息 id（0 = 从未读）；未读 = 水位之后、非本人、未软删的消息，
-- 提及数再从其中按 message_mentions 筛。updated_at 供排查与增量同步。
-- 回填存量是必要的：升级瞬间不写水位，所有历史消息都会变成未读。
-- 1) 服务器文本频道按 server_members 展开；2) direct/group 按 channel_members 展开。
-- 两个 INSERT 的频道作用域互斥，不会撞主键；无消息的频道水位落 0。
CREATE TABLE read_states (
    user_id              BIGINT   NOT NULL,
    channel_id           BIGINT   NOT NULL,
    last_read_message_id BIGINT   NOT NULL DEFAULT 0,
    updated_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, channel_id),
    KEY idx_read_states_channel (channel_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO read_states (user_id, channel_id, last_read_message_id)
SELECT sm.user_id, c.id, COALESCE(MAX(m.id), 0)
  FROM channels c
  JOIN server_members sm ON sm.server_id = c.server_id
  LEFT JOIN messages m ON m.channel_id = c.id
 WHERE c.scope = 'server' AND c.type = 'text' AND c.server_id IS NOT NULL
 GROUP BY sm.user_id, c.id;

INSERT INTO read_states (user_id, channel_id, last_read_message_id)
SELECT cm.user_id, c.id, COALESCE(MAX(m.id), 0)
  FROM channels c
  JOIN channel_members cm ON cm.channel_id = c.id
  LEFT JOIN messages m ON m.channel_id = c.id
 WHERE c.scope IN ('direct', 'group')
 GROUP BY cm.user_id, c.id;
