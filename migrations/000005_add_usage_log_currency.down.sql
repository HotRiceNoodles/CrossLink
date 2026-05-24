DROP INDEX IF EXISTS idx_usage_logs_currency;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS currency;
