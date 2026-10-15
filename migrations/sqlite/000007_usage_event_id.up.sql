-- F34: stable idempotency key for billing events (see postgres/000082).
ALTER TABLE usage_logs ADD COLUMN event_id TEXT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_logs_event_id ON usage_logs(event_id);
