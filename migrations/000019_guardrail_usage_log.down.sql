DROP INDEX IF EXISTS idx_usage_logs_guardrail;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS guardrail_rule;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS guardrail_triggered;
