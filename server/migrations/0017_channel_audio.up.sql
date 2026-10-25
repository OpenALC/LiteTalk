-- 0017_channel_audio.up.sql — 语音频道音质配置（#46）
-- audio_bitrate_kbps：0 = 不限制（跟随客户端用户设置），8–128 = 上限；
-- audio_frame_ms：0 = 默认 20ms，10/20 = 指定帧长。两列只对 voice 频道有意义，
-- text 频道恒为 0；REST 层负责校验并归一，store 原样落库。
ALTER TABLE channels
    ADD COLUMN audio_bitrate_kbps INT NOT NULL DEFAULT 0,
    ADD COLUMN audio_frame_ms INT NOT NULL DEFAULT 0;
