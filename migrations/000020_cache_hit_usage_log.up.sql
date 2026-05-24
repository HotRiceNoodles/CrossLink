ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cache_hit BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_usage_logs_cache_hit ON usage_logs (cache_hit) WHERE cache_hit = true;
