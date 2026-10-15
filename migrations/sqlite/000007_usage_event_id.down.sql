DROP INDEX IF EXISTS idx_usage_logs_event_id;
ALTER TABLE usage_logs DROP COLUMN event_id;
