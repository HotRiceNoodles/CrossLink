DROP INDEX idx_usage_logs_event_id ON usage_logs;
ALTER TABLE usage_logs DROP COLUMN event_id;
