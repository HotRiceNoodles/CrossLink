-- F34: stable idempotency key for billing events (see postgres/000082).
ALTER TABLE usage_logs ADD COLUMN event_id VARCHAR(36) NULL;
CREATE UNIQUE INDEX idx_usage_logs_event_id ON usage_logs(event_id);
