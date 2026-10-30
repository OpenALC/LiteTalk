package store

import (
	"context"
	"testing"
)

// #46：频道音质迁移与三实现契约（码率上限 + 帧长）。

// TestChannelAudioMigrationSQLite 0017 up/down/up：旧行默认跟随设置/默认帧长。
func TestChannelAudioMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	until0017 := migrationsBefore(t, full, "0018")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0017")); err != nil {
		t.Fatalf("迁移到 0016 失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES (9401, 'a@ex.test', 'a9401', '0001', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO servers (id, name, owner_id) VALUES (9501, 's', 9401)`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, position) VALUES (9601, 9501, 'old', 'voice', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx, until0017); err != nil {
		t.Fatalf("应用 0017 失败: %v", err)
	}
	var bitrate, frameMS int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT audio_bitrate_kbps, audio_frame_ms FROM channels WHERE id = 9601`).
		Scan(&bitrate, &frameMS); err != nil {
		t.Fatal(err)
	}
	if bitrate != 0 || frameMS != 0 {
		t.Fatalf("旧行默认值不符: bitrate=%d frame=%d", bitrate, frameMS)
	}

	// down：两列消失。
	if err := m.RollbackLast(ctx, until0017); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	var cols int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name IN ('audio_bitrate_kbps','audio_frame_ms')`).
		Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("down 后两列应消失: %d %v", cols, err)
	}
	// 再 up（迁移可重放）。
	if err := m.Migrate(ctx, until0017); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT audio_bitrate_kbps, audio_frame_ms FROM channels WHERE id = 9601`).
		Scan(&bitrate, &frameMS); err != nil {
		t.Fatal(err)
	}
}

func TestChannelAudioContractMem(t *testing.T) {
	runChannelAudioContract(t, NewMem())
}

func TestChannelAudioContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runChannelAudioContract(t, sq)
}

func TestChannelAudioContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runChannelAudioContract(t, m)
}

// runChannelAudioContract 三实现共用：创建落库、读回、全行更新（含清零）、text 默认 0/0。
func runChannelAudioContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	srv := &Server{Name: "audio", OwnerID: 1, IconName: "music", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	voice := &Channel{ServerID: srv.ID, Name: "voice", Type: ChannelVoice,
		AudioBitrateKbps: 16, AudioFrameMS: 10}
	if err := st.CreateChannel(ctx, voice); err != nil {
		t.Fatalf("CreateChannel(voice): %v", err)
	}
	got, err := st.ChannelByID(ctx, voice.ID)
	if err != nil {
		t.Fatalf("ChannelByID: %v", err)
	}
	if got.AudioBitrateKbps != 16 || got.AudioFrameMS != 10 {
		t.Fatalf("音质字段读回不符: %+v", got)
	}

	// 全行写回到默认（0/0），不能静默保留旧值。
	upd := *got
	upd.AudioBitrateKbps = 0
	upd.AudioFrameMS = 0
	if err := st.UpdateChannel(ctx, &upd); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	got, err = st.ChannelByID(ctx, voice.ID)
	if err != nil || got.AudioBitrateKbps != 0 || got.AudioFrameMS != 0 {
		t.Fatalf("清零后读回不符: %+v %v", got, err)
	}

	// text 频道缺省两列均为 0。
	text := &Channel{ServerID: srv.ID, Name: "text", Type: ChannelText}
	if err := st.CreateChannel(ctx, text); err != nil {
		t.Fatalf("CreateChannel(text): %v", err)
	}
	got, err = st.ChannelByID(ctx, text.ID)
	if err != nil {
		t.Fatalf("ChannelByID(text): %v", err)
	}
	if got.AudioBitrateKbps != 0 || got.AudioFrameMS != 0 {
		t.Fatalf("text 默认音质字段应为 0/0: %+v", got)
	}
}
