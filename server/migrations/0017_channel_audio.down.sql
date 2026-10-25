-- 0017_channel_audio.down.sql — 逆序删除音质两列（#46）。
ALTER TABLE channels
    DROP COLUMN audio_frame_ms,
    DROP COLUMN audio_bitrate_kbps;
