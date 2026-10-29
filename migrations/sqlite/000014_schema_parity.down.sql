-- Reverse of 000014_schema_parity. Role rows are removed only when still
-- system rows with the canonical names (permissions granted at boot die with
-- them via role_permissions rows).
DELETE FROM role_permissions WHERE role_id IN (SELECT id FROM roles WHERE name IN ('admin','member','viewer') AND is_system = 1);
DELETE FROM roles WHERE name IN ('admin','member','viewer') AND is_system = 1;

ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_system_tokens;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_history_tokens;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_question_tokens;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_tool_tokens;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_tool_output_tokens;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_total_window;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_analyzed_count;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_overflow_count;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_window_unknown_count;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_util_bucket_lt50;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_util_bucket_50_80;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_util_bucket_80_95;
ALTER TABLE datalens_daily_metrics DROP COLUMN ctx_util_bucket_gt95;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_system_tokens;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_history_tokens;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_question_tokens;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_tool_tokens;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_tool_output_tokens;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_total_window;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_analyzed_count;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_overflow_count;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_window_unknown_count;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_util_bucket_lt50;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_util_bucket_50_80;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_util_bucket_80_95;
ALTER TABLE datalens_hourly_metrics DROP COLUMN ctx_util_bucket_gt95;

DROP INDEX IF EXISTS idx_users_sso;
ALTER TABLE users DROP COLUMN sso_provider_id;
ALTER TABLE users DROP COLUMN sso_id;

DROP INDEX IF EXISTS idx_pgar_org_enabled;
DROP TABLE IF EXISTS provider_guard_alert_rules;
DROP INDEX IF EXISTS prompt_templates_name_key;
DROP INDEX IF EXISTS idx_prompt_templates_deleted_at;
DROP TABLE IF EXISTS prompt_templates;
DROP INDEX IF EXISTS idx_pat_tokens_user_id;
DROP TABLE IF EXISTS pat_tokens;
DROP TABLE IF EXISTS sso_providers;
DROP INDEX IF EXISTS mock_fixtures_lookup;
DROP INDEX IF EXISTS mock_fixtures_provider;
DROP TABLE IF EXISTS mock_fixtures;
