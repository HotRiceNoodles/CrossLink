-- Agent Shield: usage_logs extension
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS agent_type VARCHAR(32);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS security_events JSONB DEFAULT '[]';

CREATE INDEX IF NOT EXISTS idx_usage_logs_agent_type ON usage_logs (agent_type) WHERE agent_type IS NOT NULL;

-- Agent Shield: guardrail_alert_logs extension
ALTER TABLE guardrail_alert_logs ADD COLUMN IF NOT EXISTS agent_type VARCHAR(32);

-- Agent Shield: RBAC permissions
INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'agent_shield:view'
FROM roles r
WHERE r.name IN ('admin', 'member', 'viewer')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'agent_shield:manage'
FROM roles r
WHERE r.name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;
