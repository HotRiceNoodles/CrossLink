-- Agent 工具集 M2:变化告警规则(docs/plans/2026-10-28-agent-toolset-m2.md)(MySQL 方言)。
-- org_id NULL = 全局回退;event_types CSV;channels JSON 同 guardrail 告警规则;规则级冷却。
CREATE TABLE IF NOT EXISTS agent_toolset_alert_rules (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id BIGINT NULL,
    event_types VARCHAR(64) NOT NULL DEFAULT '',
    channels JSON NOT NULL,
    cooldown_minutes INT NOT NULL DEFAULT 5,
    enabled TINYINT(1) NOT NULL DEFAULT 1,
    last_triggered_at TIMESTAMP(3) NULL,
    created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    INDEX idx_tsar_org_enabled (org_id, enabled)
);

-- RBAC:告警规则管理动作(000027 guardrail_alert 体例;admin 全套,member/viewer 只读)。
INSERT IGNORE INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset_alert:delete'),
    ((SELECT id FROM roles WHERE name = 'member'), 'toolset_alert:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'toolset_alert:list');
