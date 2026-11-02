package store

// webhooks_test.go 出站订阅 creator_id（迁移 0022，P1-5）的方言级往返：
// 写入/读回一致、缺省为 0、真实 NULL 列（0022 前旧行）扫描归零——NULL 归零是
// 投递侧「owner 代持」判定的输入，方言扫描路径必须覆盖（Mem 不经过 sql.NullInt64）。

import (
	"context"
	"testing"
)

// assertOutboundWebhookCreatorRoundTrip 两种后端共用的断言体。
func assertOutboundWebhookCreatorRoundTrip(t *testing.T, st Store, ctx context.Context) {
	t.Helper()
	srv := &Server{Name: "wh-srv", OwnerID: 7}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	w := &OutboundWebhook{ServerID: srv.ID, URL: "https://hooks.example.com/a",
		Secret: "s", Events: "message.created", CreatorID: 42}
	if err := st.CreateOutboundWebhook(ctx, w); err != nil {
		t.Fatalf("CreateOutboundWebhook: %v", err)
	}
	got, err := st.OutboundWebhookByID(ctx, w.ID)
	if err != nil || got.CreatorID != 42 {
		t.Fatalf("creator_id 应往返一致: %v %+v", err, got)
	}
	// 缺省创建（代码路径未记创建者的兜底形态）读回 0。
	w2 := &OutboundWebhook{ServerID: srv.ID, URL: "https://hooks.example.com/b",
		Secret: "s", Events: "member.joined"}
	if err := st.CreateOutboundWebhook(ctx, w2); err != nil {
		t.Fatalf("CreateOutboundWebhook: %v", err)
	}
	got2, err := st.OutboundWebhookByID(ctx, w2.ID)
	if err != nil || got2.CreatorID != 0 {
		t.Fatalf("缺省 creator_id 应为 0: %v %+v", err, got2)
	}
	// 列表走同一列集，creator_id 一并带出。
	rows, err := st.OutboundWebhooksOfServer(ctx, srv.ID)
	if err != nil || len(rows) != 2 || rows[0].CreatorID != 42 || rows[1].CreatorID != 0 {
		t.Fatalf("OfServer 应带出 creator_id: %v %+v", err, rows)
	}

	// P1-8：UPDATE 同时写 creator_id——改址改绑后读回新创建者，url 与 creator 在同一
	// 条语句里落库，不会出现「新 url 配旧创建者」的中间态。
	got.URL = "https://hooks.example.com/a-rebound"
	got.CreatorID = 77
	if err := st.UpdateOutboundWebhook(ctx, got); err != nil {
		t.Fatalf("UpdateOutboundWebhook: %v", err)
	}
	got3, err := st.OutboundWebhookByID(ctx, w.ID)
	if err != nil || got3.CreatorID != 77 || got3.URL != "https://hooks.example.com/a-rebound" {
		t.Fatalf("UPDATE 应一并写入 creator_id 与 url: %v %+v", err, got3)
	}
	// 读-改-写的非改址更新（只动 events）带着读回的 creator 原样写回，创建者不变。
	got3.Events = "member.joined"
	if err := st.UpdateOutboundWebhook(ctx, got3); err != nil {
		t.Fatalf("UpdateOutboundWebhook(events): %v", err)
	}
	got4, err := st.OutboundWebhookByID(ctx, w.ID)
	if err != nil || got4.CreatorID != 77 || got4.Events != "member.joined" {
		t.Fatalf("非改址更新不应改动 creator_id: %v %+v", err, got4)
	}
	// 同服其它订阅不受影响。
	other, err := st.OutboundWebhookByID(ctx, w2.ID)
	if err != nil || other.CreatorID != 0 || other.URL != "https://hooks.example.com/b" {
		t.Fatalf("UPDATE 只作用于目标订阅: %v %+v", err, other)
	}
}

// assertNullCreatorRowRebind 真实 NULL creator_id 的旧行（0022 前、服务器已删的孤儿
// 行）经 UPDATE 改绑：NULL → 具体 uid 必须落库（P1-8 改址后旧行第一次获得创建者）。
func assertNullCreatorRowRebind(t *testing.T, st Store, ctx context.Context, id int64) {
	t.Helper()
	got, err := st.OutboundWebhookByID(ctx, id)
	if err != nil || got.CreatorID != 0 {
		t.Fatalf("前提：NULL creator_id 应扫描为 0: %v %+v", err, got)
	}
	got.CreatorID = 5
	if err := st.UpdateOutboundWebhook(ctx, got); err != nil {
		t.Fatalf("UpdateOutboundWebhook(NULL 行): %v", err)
	}
	after, err := st.OutboundWebhookByID(ctx, id)
	if err != nil || after.CreatorID != 5 {
		t.Fatalf("NULL 行应能改绑为具体 uid: %v %+v", err, after)
	}
}

// TestOutboundWebhookCreatorRoundTrip 内存与 SQLite 后端（无条件跑，外部服务零依赖）。
// SQLite 走完整迁移：0022 的 ALTER/UPDATE 与 down 的 DROP COLUMN 由迁移矩阵测试
// 覆盖，这里只补 CRUD 列往返。
func TestOutboundWebhookCreatorRoundTrip(t *testing.T) {
	ctx := context.Background()
	t.Run("mem", func(t *testing.T) {
		assertOutboundWebhookCreatorRoundTrip(t, NewMem(), ctx)
	})
	t.Run("sqlite", func(t *testing.T) {
		sq := openTestSQLiteWithMigrate(t)
		assertOutboundWebhookCreatorRoundTrip(t, sq, ctx)
		// 真实 NULL 列（0022 前旧行）：迁移回填只对存量行跑过一次，之后服务器已删
		// 的孤儿行仍是 NULL——扫描必须归零而不是报错。id 直插绕过 Store 回填。
		if _, err := sq.DB().ExecContext(ctx,
			`INSERT INTO webhooks (id, server_id, url, secret, events) VALUES (999, 1, 'https://u.example', 's', 'message.created')`); err != nil {
			t.Fatalf("裸插 NULL creator 行: %v", err)
		}
		got, err := sq.OutboundWebhookByID(ctx, 999)
		if err != nil || got.CreatorID != 0 {
			t.Fatalf("NULL creator_id 应扫描为 0: %v %+v", err, got)
		}
		assertNullCreatorRowRebind(t, sq, ctx, 999)
	})
}

// TestOutboundWebhookCreatorRoundTripMySQL 真库矩阵（MySQL 不可用自动跳过，与既有真库测试同口径）。
func TestOutboundWebhookCreatorRoundTripMySQL(t *testing.T) {
	m := openTestMySQL(t)
	defer m.Close()
	ctx := context.Background()
	if _, err := m.DB().ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS litetalk_test"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx, "USE litetalk_test"); err != nil {
		t.Fatal(err)
	}
	// 清理历史：与 TestBotsAmongMySQL 同清单（webhooks 三表在列）。
	for _, tbl := range []string{
		"attachments", "read_states", "invites", "messages", "message_mentions", "channel_members", "channels", "categories", "server_members",
		"servers", "friendships", "users", "user_sessions", "kv", "schema_migrations",
		"audit_log", "channel_overwrites", "member_roles", "roles", "api_tokens",
		"push_devices",
		"webhook_deliveries", "inbound_webhooks", "webhooks",
		"oidc_identities",
	} {
		_, _ = m.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}
	if err := m.Migrate(ctx, findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	assertOutboundWebhookCreatorRoundTrip(t, m, ctx)
	// MySQL 侧 NULL 扫描同口径补一条（BIGINT NULL → sql.NullInt64）。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO webhooks (id, server_id, url, secret, events) VALUES (999, 1, 'https://u.example', 's', 'message.created')`); err != nil {
		t.Fatalf("裸插 NULL creator 行: %v", err)
	}
	got, err := m.OutboundWebhookByID(ctx, 999)
	if err != nil || got.CreatorID != 0 {
		t.Fatalf("NULL creator_id 应扫描为 0: %v %+v", err, got)
	}
	assertNullCreatorRowRebind(t, m, ctx, 999)
}
