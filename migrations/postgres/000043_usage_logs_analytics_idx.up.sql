CREATE INDEX IF NOT EXISTS idx_usage_logs_status_code ON usage_logs(status_code);
CREATE INDEX IF NOT EXISTS idx_usage_logs_error_type ON usage_logs(error_type) WHERE error_type IS NOT NULL;
