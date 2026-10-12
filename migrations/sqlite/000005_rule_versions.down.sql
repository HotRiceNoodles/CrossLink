ALTER TABLE guardrail_alert_logs DROP COLUMN matched;
ALTER TABLE guardrail_alert_logs DROP COLUMN rule_version;
DROP TABLE guardrail_rule_versions;
ALTER TABLE guardrail_rules DROP COLUMN version;
