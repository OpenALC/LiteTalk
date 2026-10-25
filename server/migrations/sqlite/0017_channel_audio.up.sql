-- 0017_channel_audio.up.sql — 语音频道音质配置（#46；SQLite 方言）
-- 语义与 MySQL 版一致：bitrate 0 = 跟随客户端设置，8–128 = 上限；
-- frame_ms 0 = 默认 20ms，10/20 = 指定帧长。text 频道恒为 0/0。
ALTER TABLE channels ADD COLUMN audio_bitrate_kbps INT NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN audio_frame_ms INT NOT NULL DEFAULT 0;
