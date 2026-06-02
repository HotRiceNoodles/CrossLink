ALTER TABLE usage_logs ADD COLUMN retry_count SMALLINT NOT NULL DEFAULT 0;
CREATE INDEX idx_usage_logs_retry ON usage_logs(retry_count) WHERE retry_count > 0;
