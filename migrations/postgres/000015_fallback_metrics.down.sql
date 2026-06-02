DROP INDEX IF EXISTS idx_usage_logs_fallback;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS fallback_count;
