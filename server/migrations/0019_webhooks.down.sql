-- 0019_webhooks.down.sql — 逆序回滚（#52）
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS inbound_webhooks;
DROP TABLE IF EXISTS webhooks;
