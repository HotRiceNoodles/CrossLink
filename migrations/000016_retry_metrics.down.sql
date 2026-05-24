DROP INDEX IF EXISTS idx_usage_logs_retry;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS retry_count;
