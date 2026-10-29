-- Agent 工具集 M2:变化告警规则(docs/plans/2026-10-28-agent-toolset-m2.md)。
-- org_id NULL = 全局回退规则;event_types CSV(first_seen/definition_changed,空=两者);
-- channels JSON 同 guardrail 告警规则;冷却为规则级(同 provider_guard 体例)。
CREATE TABLE IF NOT EXISTS agent_toolset_alert_rules (
    id                 BIGSERIAL PRIMARY KEY,
    org_id             BIGINT,
    event_types        VARCHAR(64) NOT NULL DEFAULT '',
    channels           JSONB NOT NULL DEFAULT '[]',
    cooldown_minutes   INT NOT NULL DEFAULT 5,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    last_triggered_at  TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tsar_org_enabled ON agent_toolset_alert_rules(org_id, enabled);

-- RBAC:告警规则管理动作(000027 guardrail_alert 体例;admin 全套,member/viewer 只读)。
INSERT INTO role_permissions (role_id, action)
SELECT r.id, a.action
FROM roles r
CROSS JOIN (VALUES ('toolset_alert:list'), ('toolset_alert:create'), ('toolset_alert:update'), ('toolset_alert:delete')) AS a(action)
WHERE r.name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'toolset_alert:list'
FROM roles r WHERE r.name IN ('member', 'viewer')
ON CONFLICT (role_id, action) DO NOTHING;
