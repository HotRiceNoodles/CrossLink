DROP INDEX IF EXISTS idx_usage_logs_session_id;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS reasoning_tokens;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS cache_read_tokens;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS session_id;
