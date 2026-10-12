-- Reversible: drops the versioning artifacts. History rows are lost on
-- downgrade (documented; the down migration is a schema rollback, not a
-- history edit path).
ALTER TABLE guardrail_alert_logs DROP COLUMN IF EXISTS matched;
ALTER TABLE guardrail_alert_logs DROP COLUMN IF EXISTS rule_version;
DROP TABLE IF EXISTS guardrail_rule_versions;
ALTER TABLE guardrail_rules DROP COLUMN IF EXISTS version;
