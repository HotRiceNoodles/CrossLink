-- UsageLog model parity for MySQL (round-12 authorization): the squashed
-- init (000001, historical, untouched) predates these columns; PostgreSQL
-- carries them in its incremental chain (000070-000077). This migration is
-- their sole executor for MySQL — nothing here may also live in 000001.
ALTER TABLE usage_logs ADD COLUMN billable_cost DECIMAL(16,8) NOT NULL DEFAULT 0;
ALTER TABLE usage_logs ADD COLUMN error_message TEXT;
ALTER TABLE usage_logs ADD COLUMN upstream_status INT;
ALTER TABLE usage_logs ADD COLUMN upstream_error_code VARCHAR(64);
ALTER TABLE usage_logs ADD COLUMN upstream_error_type VARCHAR(64);
ALTER TABLE usage_logs ADD COLUMN upstream_error_param VARCHAR(128);
ALTER TABLE usage_logs ADD COLUMN error_stage VARCHAR(16);
ALTER TABLE usage_logs ADD COLUMN attempts JSON;
ALTER TABLE usage_logs ADD COLUMN template_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN image_count BIGINT;
ALTER TABLE usage_logs ADD COLUMN image_size VARCHAR(16);
ALTER TABLE usage_logs ADD COLUMN image_quality VARCHAR(16);
ALTER TABLE usage_logs ADD COLUMN system_tokens INT;
ALTER TABLE usage_logs ADD COLUMN history_tokens INT;
ALTER TABLE usage_logs ADD COLUMN question_tokens INT;
ALTER TABLE usage_logs ADD COLUMN tool_tokens INT;
ALTER TABLE usage_logs ADD COLUMN tool_output_tokens INT;
ALTER TABLE usage_logs ADD COLUMN context_window INT;
ALTER TABLE usage_logs ADD COLUMN context_utilization_bp INT;
ALTER TABLE usage_logs ADD COLUMN analysis_flags INT;
ALTER TABLE usage_logs ADD COLUMN context_snapshot JSON;
CREATE INDEX idx_usage_logs_template_id ON usage_logs(template_id);
