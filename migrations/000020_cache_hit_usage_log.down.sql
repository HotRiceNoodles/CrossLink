DROP INDEX IF EXISTS idx_usage_logs_cache_hit;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS cache_hit;
