-- U-08 follow-up: the fallback composite (fallback_count, created_at) only
-- landed on MySQL (000008); the admin usage list (has_fallback=true) and the
-- commercial fallback panel both filter created_at >= ? AND fallback_count
-- > 0, so PG needs the same index. Plain CREATE
-- INDEX matches the set's established form; on very large production tables
-- run this file's statements manually with CONCURRENTLY instead.
CREATE INDEX IF NOT EXISTS idx_usage_logs_fallback_created ON usage_logs (fallback_count, created_at);
