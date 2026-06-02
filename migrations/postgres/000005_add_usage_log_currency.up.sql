ALTER TABLE usage_logs ADD COLUMN currency VARCHAR(3) NOT NULL DEFAULT 'CNY';
CREATE INDEX idx_usage_logs_currency ON usage_logs(currency);
