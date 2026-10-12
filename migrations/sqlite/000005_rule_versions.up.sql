-- M3-A: rule versioning (2026-10-13). See postgres/000080 for the design.
ALTER TABLE guardrail_rules ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

CREATE TABLE guardrail_rule_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id INTEGER NOT NULL,
    version INTEGER NOT NULL,
    org_id INTEGER,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    direction TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    config TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'medium',
    action TEXT NOT NULL DEFAULT 'block',
    model_filter TEXT,
    change_kind TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (rule_id, version)
);
CREATE INDEX idx_rule_versions_rule ON guardrail_rule_versions(rule_id);

INSERT INTO guardrail_rule_versions
    (rule_id, version, org_id, name, type, direction, enabled, config, severity, action, model_filter, change_kind, actor)
SELECT id, 1, org_id, name, type, direction, enabled, config, severity, action, model_filter, 'migrate', 'migration'
FROM guardrail_rules
WHERE NOT EXISTS (
    SELECT 1 FROM guardrail_rule_versions h WHERE h.rule_id = guardrail_rules.id
);

ALTER TABLE guardrail_alert_logs ADD COLUMN rule_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE guardrail_alert_logs ADD COLUMN matched TEXT;
