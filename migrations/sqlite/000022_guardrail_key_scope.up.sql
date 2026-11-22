ALTER TABLE guardrail_rules ADD COLUMN scope_mode VARCHAR(12) NOT NULL DEFAULT 'all';
ALTER TABLE guardrail_rules ADD COLUMN api_key_ids TEXT;
ALTER TABLE guardrail_rule_versions ADD COLUMN scope_mode VARCHAR(12) NOT NULL DEFAULT 'all';
ALTER TABLE guardrail_rule_versions ADD COLUMN api_key_ids TEXT;
