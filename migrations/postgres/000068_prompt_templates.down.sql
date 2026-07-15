-- Reverse migration 000068.
DROP INDEX IF EXISTS idx_usage_logs_template_id;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS template_id;

DROP INDEX IF EXISTS idx_prompt_templates_deleted_at;
DROP INDEX IF EXISTS prompt_templates_name_key;
DROP TABLE IF EXISTS prompt_templates;
