ALTER TABLE usage_logs ADD COLUMN reasoning_tokens INT NOT NULL DEFAULT 0;
ALTER TABLE usage_logs ADD COLUMN cache_read_tokens INT NOT NULL DEFAULT 0;
ALTER TABLE usage_logs ADD COLUMN session_id VARCHAR(255);
CREATE INDEX idx_usage_logs_session_id ON usage_logs(session_id);
