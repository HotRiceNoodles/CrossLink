-- Fallback metrics: track fallback count
ALTER TABLE usage_logs ADD COLUMN fallback_count SMALLINT NOT NULL DEFAULT 0;

-- Partial index for fallback analysis (only indexes rows with fallbacks)
CREATE INDEX idx_usage_logs_fallback ON usage_logs(fallback_count) WHERE fallback_count > 0;
