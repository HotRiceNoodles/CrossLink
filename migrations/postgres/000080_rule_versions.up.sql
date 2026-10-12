-- M3-A: rule versioning (2026-10-13). Per-rule monotonic version column,
-- append-only history table with a one-time snapshot backfill of pre-existing
-- rules (change_kind='migrate'), and event-attribution columns on the alert
-- log. History rows are INSERT-only; no code path updates or deletes them.
ALTER TABLE guardrail_rules ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS guardrail_rule_versions (
    id BIGSERIAL PRIMARY KEY,
    rule_id BIGINT NOT NULL,
    version BIGINT NOT NULL,
    org_id BIGINT,
    name VARCHAR(255) NOT NULL,
    type VARCHAR(50) NOT NULL,
    direction VARCHAR(10) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    config JSONB NOT NULL,
    severity VARCHAR(20) NOT NULL DEFAULT 'medium',
    action VARCHAR(20) NOT NULL DEFAULT 'block',
    model_filter TEXT,
    change_kind VARCHAR(20) NOT NULL,
    actor VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_rule_versions UNIQUE (rule_id, version)
);
CREATE INDEX IF NOT EXISTS idx_rule_versions_rule ON guardrail_rule_versions(rule_id);

-- Initial snapshot for rules that predate the versioned write path. Runs
-- once per fresh upgrade; safe on new installs (empty table → no rows).
INSERT INTO guardrail_rule_versions
    (rule_id, version, org_id, name, type, direction, enabled, config, severity, action, model_filter, change_kind, actor)
SELECT id, 1, org_id, name, type, direction, enabled, config, severity, action, model_filter, 'migrate', 'migration'
FROM guardrail_rules
WHERE NOT EXISTS (
    SELECT 1 FROM guardrail_rule_versions h WHERE h.rule_id = guardrail_rules.id
);

ALTER TABLE guardrail_alert_logs ADD COLUMN IF NOT EXISTS rule_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE guardrail_alert_logs ADD COLUMN IF NOT EXISTS matched JSONB;
