ALTER TABLE usage_logs ADD COLUMN service_tier VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE usage_logs ADD COLUMN tools_used JSON;
