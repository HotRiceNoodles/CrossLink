-- Reverse of 000012_schema_parity. Role rows are removed only when still
-- system rows with the canonical names (permissions granted at boot die with
-- them via role_permissions FK cascade).
DELETE FROM role_permissions WHERE role_id IN (SELECT id FROM roles WHERE name IN ('admin','member','viewer') AND is_system = 1);
DELETE FROM roles WHERE name IN ('admin','member','viewer') AND is_system = 1;

ALTER TABLE datalens_daily_metrics
    DROP COLUMN ctx_system_tokens, DROP COLUMN ctx_history_tokens,
    DROP COLUMN ctx_question_tokens, DROP COLUMN ctx_tool_tokens,
    DROP COLUMN ctx_tool_output_tokens, DROP COLUMN ctx_total_window,
    DROP COLUMN ctx_analyzed_count, DROP COLUMN ctx_overflow_count,
    DROP COLUMN ctx_window_unknown_count, DROP COLUMN ctx_util_bucket_lt50,
    DROP COLUMN ctx_util_bucket_50_80, DROP COLUMN ctx_util_bucket_80_95,
    DROP COLUMN ctx_util_bucket_gt95;
ALTER TABLE datalens_hourly_metrics
    DROP COLUMN ctx_system_tokens, DROP COLUMN ctx_history_tokens,
    DROP COLUMN ctx_question_tokens, DROP COLUMN ctx_tool_tokens,
    DROP COLUMN ctx_tool_output_tokens, DROP COLUMN ctx_total_window,
    DROP COLUMN ctx_analyzed_count, DROP COLUMN ctx_overflow_count,
    DROP COLUMN ctx_window_unknown_count, DROP COLUMN ctx_util_bucket_lt50,
    DROP COLUMN ctx_util_bucket_50_80, DROP COLUMN ctx_util_bucket_80_95,
    DROP COLUMN ctx_util_bucket_gt95;

ALTER TABLE api_keys DROP COLUMN price_multiplier;
ALTER TABLE api_keys DROP COLUMN key_source;
ALTER TABLE api_keys DROP COLUMN source_vendor;

DROP INDEX idx_users_sso ON users;
ALTER TABLE users DROP COLUMN sso_provider_id;
ALTER TABLE users DROP COLUMN sso_id;

DROP INDEX idx_pgar_org_enabled ON provider_guard_alert_rules;
DROP TABLE IF EXISTS provider_guard_alert_rules;
DROP INDEX idx_prompt_templates_name ON prompt_templates;
DROP INDEX idx_prompt_templates_deleted_at ON prompt_templates;
DROP TABLE IF EXISTS prompt_templates;
DROP INDEX idx_pat_tokens_user_id ON pat_tokens;
DROP TABLE IF EXISTS pat_tokens;
DROP TABLE IF EXISTS sso_providers;
DROP INDEX idx_mock_fixtures_lookup ON mock_fixtures;
DROP INDEX idx_mock_fixtures_provider ON mock_fixtures;
DROP TABLE IF EXISTS mock_fixtures;
