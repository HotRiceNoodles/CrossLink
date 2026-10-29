-- Agent 工具集 M2:变化告警规则(docs/plans/2026-10-28-agent-toolset-m2.md)(SQLite 方言)。
-- org_id NULL = 全局回退;event_types CSV;channels JSON 存 TEXT;规则级冷却。
CREATE TABLE IF NOT EXISTS agent_toolset_alert_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id INTEGER,
    event_types TEXT NOT NULL DEFAULT '',
    channels TEXT NOT NULL DEFAULT '[]',
    cooldown_minutes INTEGER NOT NULL DEFAULT 5,
    enabled INTEGER NOT NULL DEFAULT 1,
    last_triggered_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_tsar_org_enabled ON agent_toolset_alert_rules(org_id, enabled);

-- RBAC:告警规则管理动作(000027 guardrail_alert 体例;admin 全套,member/viewer 只读)。
INSERT OR IGNORE INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:delete'),
    ((SELECT id FROM roles WHERE name = 'member'), 'toolset_alert:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'toolset_alert:list');
