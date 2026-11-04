-- U-08 follow-up: the fallback composite (fallback_count, created_at) only
-- landed on MySQL (000008); the admin usage list (has_fallback=true) filters
-- created_at >= ? AND fallback_count > 0 on every dialect. 000018 is
-- reserved for the commercial overlay's insights migration — the community
-- sqlite chain skips it. SQLite deployments are small-scale; index build
-- cost is trivial.
CREATE INDEX IF NOT EXISTS idx_usage_logs_fallback_created ON usage_logs (fallback_count, created_at);
