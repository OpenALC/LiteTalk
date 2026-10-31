package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// webhooks（迁移 0019，#52）：出站订阅、入站投递入口与投递记录。
// MySQL/SQLite 的列集与 SQL 完全同形（无 ENUM、无方言 upsert），CRUD 走本文件的
// db* 共享实现，两个方言只做事务/连接接线，避免语义漂移。

// OutboundWebhook 出站订阅（服务器级）。Secret 是 HMAC-SHA256 签名密钥（64 位 hex），
// 只写不读回：明文只在创建响应与 reset_secret 的 PATCH 响应里出现一次，任何查询
// 路径都不得回传。Events 为空格分隔的事件白名单子集（校验在 internal/webhook）。
// Disabled/DisabledReason/FailCount 是熔断状态：连续失败达阈值由
// RecordWebhookDelivery 原子置位，手动重新启用时由 API 层清零。
type OutboundWebhook struct {
	ID             int64
	ServerID       int64
	URL            string
	Secret         string
	Events         string
	Disabled       bool
	DisabledReason string
	FailCount      int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// InboundWebhook 入站投递入口（频道级）。Secret 参与 URL（/api/v1/webhooks/{id}/{secret}），
// 校验必须常数时间比对；泄露即重建（旧 secret 校验失败按不存在处理，不区分原因）。
type InboundWebhook struct {
	ID          int64
	ChannelID   int64
	Secret      string
	Description string
	CreatedAt   time.Time
}

// WebhookDelivery 一次出站投递的最终结果（重试耗尽后的终态，不是每次尝试）。
// Error 是最后一次失败的摘要（截断到 255 字节，供排障，不含 URL 查询参数）。
type WebhookDelivery struct {
	ID         int64
	WebhookID  int64
	Event      string
	Attempts   int
	OK         bool
	StatusCode int
	Error      string
	CreatedAt  time.Time
}

// 出站/入站/投递记录的查询列集（MySQL 与 SQLite 实现共用）。
const (
	outboundWebhookCols = `id, server_id, url, secret, events, disabled, disabled_reason, fail_count, created_at, updated_at`
	inboundWebhookCols  = `id, channel_id, secret, description, created_at`
	webhookDeliveryCols = `id, webhook_id, event, attempts, ok, status_code, error, created_at`
)

// dbCreateOutboundWebhook 写入一行；ID 未预置时取 Snowflake，回填时间戳。
func dbCreateOutboundWebhook(ctx context.Context, q sqlExecer, w *OutboundWebhook) error {
	w.ID = newID(w.ID)
	now := time.Now()
	w.CreatedAt, w.UpdatedAt = now, now
	_, err := q.ExecContext(ctx,
		`INSERT INTO webhooks (id, server_id, url, secret, events, disabled, disabled_reason, fail_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.ServerID, w.URL, w.Secret, w.Events, w.Disabled, w.DisabledReason, w.FailCount, now, now)
	return err
}

// dbScanOutboundWebhook 行扫描（TINYINT(1)/INTEGER 存的布尔两方言都可 Scan 进 bool）。
func dbScanOutboundWebhook(row interface{ Scan(...any) error }) (*OutboundWebhook, error) {
	var w OutboundWebhook
	err := row.Scan(&w.ID, &w.ServerID, &w.URL, &w.Secret, &w.Events,
		&w.Disabled, &w.DisabledReason, &w.FailCount, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// dbOutboundWebhookByID 主键回读。
func dbOutboundWebhookByID(ctx context.Context, q sqlExecer, id int64) (*OutboundWebhook, error) {
	return dbScanOutboundWebhook(q.QueryRowContext(ctx,
		`SELECT `+outboundWebhookCols+` FROM webhooks WHERE id = ?`, id))
}

// dbOutboundWebhooksOfServer 该服全部订阅，按 id 升序。
func dbOutboundWebhooksOfServer(ctx context.Context, q sqlExecer, serverID int64) ([]OutboundWebhook, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+outboundWebhookCols+` FROM webhooks WHERE server_id = ? ORDER BY id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboundWebhook{}
	for rows.Next() {
		w, err := dbScanOutboundWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// dbHasOutboundWebhooks 库内是否还有任一出站订阅（启动期判定是否需要投递 worker）。
func dbHasOutboundWebhooks(ctx context.Context, q sqlExecer) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM webhooks LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// dbUpdateOutboundWebhook 全行写（url/secret/events/熔断状态）；调用方读-改-写，
// 不动 created_at/server_id。未命中 ErrNotFound。
func dbUpdateOutboundWebhook(ctx context.Context, q sqlExecer, w *OutboundWebhook) error {
	w.UpdatedAt = time.Now()
	res, err := q.ExecContext(ctx,
		`UPDATE webhooks SET url = ?, secret = ?, events = ?, disabled = ?, disabled_reason = ?, fail_count = ?, updated_at = ? WHERE id = ?`,
		w.URL, w.Secret, w.Events, w.Disabled, w.DisabledReason, w.FailCount, w.UpdatedAt, w.ID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// dbDeleteOutboundWebhook 删除订阅行；未命中 ErrNotFound。投递记录保留（随保留期清理）。
func dbDeleteOutboundWebhook(ctx context.Context, q sqlExecer, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// dbRecordWebhookDelivery 落一条投递记录并在同一事务里推进熔断计数：成功清零
// fail_count；失败自增，达到 breakerAt 时置 disabled=1 与 disabled_reason（已停用的
// 保持停用，计数继续累计无副作用）。订阅已被并发删除时记录照落、计数跳过。
// 调用方（SQLite/MySQL 接线）必须以事务传入 q，读-改-写才不会被并发投递撕开。
func dbRecordWebhookDelivery(ctx context.Context, q sqlExecer, d *WebhookDelivery, breakerAt int, reason string) error {
	d.ID = newID(d.ID)
	now := time.Now()
	if _, err := q.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (id, webhook_id, event, attempts, ok, status_code, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.WebhookID, d.Event, d.Attempts, d.OK, d.StatusCode, d.Error, now); err != nil {
		return err
	}
	d.CreatedAt = now
	if d.WebhookID == 0 {
		return nil
	}
	var failCount int
	var disabled bool
	var disabledReason string
	err := q.QueryRowContext(ctx,
		`SELECT fail_count, disabled, disabled_reason FROM webhooks WHERE id = ?`, d.WebhookID).
		Scan(&failCount, &disabled, &disabledReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 订阅已删除：投递记录保留，熔断计数无行可推进
	}
	if err != nil {
		return err
	}
	if d.OK {
		failCount = 0
	} else {
		failCount++
		if !disabled && failCount >= breakerAt {
			disabled = true
			disabledReason = reason
		}
	}
	_, err = q.ExecContext(ctx,
		`UPDATE webhooks SET fail_count = ?, disabled = ?, disabled_reason = ? WHERE id = ?`,
		failCount, disabled, disabledReason, d.WebhookID)
	return err
}

// dbWebhookDeliveriesBefore 早于 before 的投递记录，按 id 升序；limit 夹到 [1,500]。
func dbWebhookDeliveriesBefore(ctx context.Context, q sqlExecer, before time.Time, limit int) ([]WebhookDelivery, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+webhookDeliveryCols+` FROM webhook_deliveries WHERE created_at < ? ORDER BY id LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookDelivery{}
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.Event, &d.Attempts, &d.OK, &d.StatusCode, &d.Error, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// dbDeleteWebhookDeliveries 批量删除投递记录；空入参 no-op，不存在的 id 忽略。
func dbDeleteWebhookDeliveries(ctx context.Context, q sqlExecer, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// 参数化 IN：批量上限 500（清理批大小），拼占位符不引入注入面（全部 int64）。
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	_, err := q.ExecContext(ctx,
		`DELETE FROM webhook_deliveries WHERE id IN (`+string(placeholders)+`)`, args...)
	return err
}

// dbCreateInboundWebhook 写入频道级投递入口；ID 未预置时取 Snowflake，回填 CreatedAt。
func dbCreateInboundWebhook(ctx context.Context, q sqlExecer, w *InboundWebhook) error {
	w.ID = newID(w.ID)
	now := time.Now()
	w.CreatedAt = now
	_, err := q.ExecContext(ctx,
		`INSERT INTO inbound_webhooks (id, channel_id, secret, description, created_at) VALUES (?, ?, ?, ?, ?)`,
		w.ID, w.ChannelID, w.Secret, w.Description, now)
	return err
}

// dbScanInboundWebhook 行扫描。
func dbScanInboundWebhook(row interface{ Scan(...any) error }) (*InboundWebhook, error) {
	var w InboundWebhook
	err := row.Scan(&w.ID, &w.ChannelID, &w.Secret, &w.Description, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// dbInboundWebhookByID 主键回读（投递入口校验热路径，走主键）。
func dbInboundWebhookByID(ctx context.Context, q sqlExecer, id int64) (*InboundWebhook, error) {
	return dbScanInboundWebhook(q.QueryRowContext(ctx,
		`SELECT `+inboundWebhookCols+` FROM inbound_webhooks WHERE id = ?`, id))
}

// dbInboundWebhooksOfChannel 该频道的全部入站入口，按 id 升序。
func dbInboundWebhooksOfChannel(ctx context.Context, q sqlExecer, channelID int64) ([]InboundWebhook, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+inboundWebhookCols+` FROM inbound_webhooks WHERE channel_id = ? ORDER BY id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboundWebhook{}
	for rows.Next() {
		w, err := dbScanInboundWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// dbDeleteInboundWebhook 删除入站入口；未命中 ErrNotFound。
func dbDeleteInboundWebhook(ctx context.Context, q sqlExecer, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM inbound_webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res)
}
