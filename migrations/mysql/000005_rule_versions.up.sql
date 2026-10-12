-- M3-A: rule versioning (2026-10-13). See postgres/000080 for the design.
ALTER TABLE guardrail_rules ADD COLUMN version BIGINT NOT NULL DEFAULT 1;

CREATE TABLE guardrail_rule_versions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    rule_id BIGINT NOT NULL,
    version BIGINT NOT NULL,
    org_id BIGINT,
    name VARCHAR(255) NOT NULL,
    type VARCHAR(50) NOT NULL,
    direction VARCHAR(10) NOT NULL,
    enabled TINYINT(1) NOT NULL DEFAULT 1,
    config JSON NOT NULL,
    severity VARCHAR(20) NOT NULL DEFAULT 'medium',
    action VARCHAR(20) NOT NULL DEFAULT 'block',
    model_filter TEXT,
    change_kind VARCHAR(20) NOT NULL,
    actor VARCHAR(64) NOT NULL DEFAULT '',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uq_rule_versions (rule_id, version),
    KEY idx_rule_versions_rule (rule_id)
);

INSERT INTO guardrail_rule_versions
    (rule_id, version, org_id, name, type, direction, enabled, config, severity, action, model_filter, change_kind, actor)
SELECT id, 1, org_id, name, type, direction, enabled, config, severity, action, model_filter, 'migrate', 'migration'
FROM guardrail_rules
WHERE NOT EXISTS (
    SELECT 1 FROM guardrail_rule_versions h WHERE h.rule_id = guardrail_rules.id
);

ALTER TABLE guardrail_alert_logs ADD COLUMN rule_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE guardrail_alert_logs ADD COLUMN matched JSON;
