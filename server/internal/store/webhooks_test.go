package store

// webhooks_test.go 出站订阅 creator_id（迁移 0022，P1-5）的方言级往返：
// 写入/读回一致、缺省为 0、真实 NULL 列（0022 前旧行）扫描归零——NULL 归零是
// 投递侧「owner 代持」判定的输入，方言扫描路径必须覆盖（Mem 不经过 sql.NullInt64）。

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
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

// ---- 熔断计数原子性（#119）----

// assertConcurrentBreaker 并发投递同一订阅：失败计数必须精确等于投递条数，
// 恰在阈值处停用；已停用后继续失败只累计计数、不改停用原因；成功记录清零计数
// 但不自动重新启用（重新启用是 PATCH enabled=true 的职责）。
// 旧实现的「SELECT 旧值 → UPDATE 绝对值」在 MySQL 多连接下会丢更新，本断言
// 会把丢失的那部分计数揪出来。
func assertConcurrentBreaker(t *testing.T, st Store, ctx context.Context) {
	t.Helper()
	const fails = 32
	w := &OutboundWebhook{ServerID: 1, URL: "https://hooks.example.com/race",
		Secret: "s", Events: "message.created", CreatorID: 1}
	if err := st.CreateOutboundWebhook(ctx, w); err != nil {
		t.Fatalf("CreateOutboundWebhook: %v", err)
	}

	// 边界：恰好 breakerAt-1 次失败不得停用。MySQL 的单表 UPDATE 从左到右求值，
	// fail_count 若写在 disabled 之前，disabled 会看到已自增的新值而提前一次熔断；
	// SQLite 则永远用旧值。这条断言钉住两方言在同一阈值边界上的一致行为。
	edge := &OutboundWebhook{ServerID: 1, URL: "https://hooks.example.com/edge",
		Secret: "s", Events: "message.created", CreatorID: 1}
	if err := st.CreateOutboundWebhook(ctx, edge); err != nil {
		t.Fatalf("CreateOutboundWebhook(edge): %v", err)
	}
	for i := 0; i < fails-1; i++ {
		if err := st.RecordWebhookDelivery(ctx, &WebhookDelivery{
			WebhookID: edge.ID, Event: "message.created", Attempts: 1,
		}, fails, "consecutive_failures"); err != nil {
			t.Fatalf("RecordWebhookDelivery(edge 第 %d 次): %v", i+1, err)
		}
	}
	if got, err := st.OutboundWebhookByID(ctx, edge.ID); err != nil ||
		got.FailCount != fails-1 || got.Disabled {
		t.Fatalf("阈值前不应停用: %v %+v", err, got)
	}
	if err := st.RecordWebhookDelivery(ctx, &WebhookDelivery{
		WebhookID: edge.ID, Event: "message.created", Attempts: 1,
	}, fails, "consecutive_failures"); err != nil {
		t.Fatalf("RecordWebhookDelivery(edge 达阈值): %v", err)
	}
	if got, err := st.OutboundWebhookByID(ctx, edge.ID); err != nil ||
		!got.Disabled || got.DisabledReason != "consecutive_failures" {
		t.Fatalf("达到阈值应停用: %v %+v", err, got)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, fails)
	for i := 0; i < fails; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- st.RecordWebhookDelivery(ctx, &WebhookDelivery{
				WebhookID: w.ID, Event: "message.created", Attempts: 1,
			}, fails, "consecutive_failures")
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发 RecordWebhookDelivery: %v", err)
		}
	}

	got, err := st.OutboundWebhookByID(ctx, w.ID)
	if err != nil {
		t.Fatalf("OutboundWebhookByID: %v", err)
	}
	if got.FailCount != fails {
		t.Fatalf("并发失败计数丢更新：want %d, got %d", fails, got.FailCount)
	}
	if !got.Disabled || got.DisabledReason != "consecutive_failures" {
		t.Fatalf("达到阈值应停用：%+v", got)
	}

	// 已停用后继续失败：计数照常累计，停用原因不被覆盖。
	if err := st.RecordWebhookDelivery(ctx, &WebhookDelivery{
		WebhookID: w.ID, Event: "message.created", Attempts: 1,
	}, fails, "consecutive_failures"); err != nil {
		t.Fatalf("RecordWebhookDelivery(停用后失败): %v", err)
	}
	if got, err = st.OutboundWebhookByID(ctx, w.ID); err != nil || got.FailCount != fails+1 ||
		!got.Disabled || got.DisabledReason != "consecutive_failures" {
		t.Fatalf("停用后失败应继续累计且保持原因: %v %+v", err, got)
	}

	// 成功记录清零计数，但不会自动重新启用。
	if err := st.RecordWebhookDelivery(ctx, &WebhookDelivery{
		WebhookID: w.ID, Event: "message.created", Attempts: 1, OK: true,
	}, fails, "consecutive_failures"); err != nil {
		t.Fatalf("RecordWebhookDelivery(成功): %v", err)
	}
	if got, err = st.OutboundWebhookByID(ctx, w.ID); err != nil || got.FailCount != 0 ||
		!got.Disabled || got.DisabledReason != "consecutive_failures" {
		t.Fatalf("成功记录应清零计数但保持停用: %v %+v", err, got)
	}
}

// TestRecordWebhookDeliveryConcurrent 内存与 SQLite 后端（无条件跑）。
// SQLite 的 MaxOpenConns(1) + BEGIN IMMEDIATE 使事务串行，本用例验证并发调用
// 下的计数完整性；真正的多连接丢更新只在 MySQL 方言上可复现（下一条用例）。
func TestRecordWebhookDeliveryConcurrent(t *testing.T) {
	ctx := context.Background()
	t.Run("mem", func(t *testing.T) {
		assertConcurrentBreaker(t, NewMem(), ctx)
	})
	t.Run("sqlite", func(t *testing.T) {
		assertConcurrentBreaker(t, openTestSQLiteWithMigrate(t), ctx)
	})
}

// openTestMySQLSchema 打开已选中 schema 的 MySQL 连接池。并发用例不能靠
// "USE litetalk_test"：USE 只作用于连接池里的某条连接，新开的连接没有默认库。
// 把库名写进 DSN，池里每条连接都带着它。MySQL 不可用时 skip。
func openTestMySQLSchema(t *testing.T, schema string) *MySQL {
	t.Helper()
	dsn := os.Getenv("LITETALK_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true"
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Skipf("MySQL DSN 不可解析，跳过真库测试: %v", err)
	}
	cfg.DBName = schema
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Skipf("MySQL 不可用，跳过真库测试: %v", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("MySQL 不可用，跳过真库测试: %v", err)
	}
	return &MySQL{db: db}
}

// TestRecordWebhookDeliveryConcurrentMySQL 真库矩阵：多连接并发写同一订阅，
// 原子自增下计数精确。MySQL 不可用自动跳过（与既有真库测试同口径；CI 的
// check-server 也没有 MySQL service，故该用例在 CI 上同样 skip）。
func TestRecordWebhookDeliveryConcurrentMySQL(t *testing.T) {
	boot := openTestMySQL(t)
	ctx := context.Background()
	if _, err := boot.DB().ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS litetalk_test"); err != nil {
		boot.Close()
		t.Fatalf("建测试库失败: %v", err)
	}
	boot.Close()
	m := openTestMySQLSchema(t, "litetalk_test")
	defer m.Close()
	// 清理历史：与 TestOutboundWebhookCreatorRoundTripMySQL 同清单。
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
	assertConcurrentBreaker(t, m, ctx)
}
