DELETE FROM role_permissions WHERE action IN ('agent_shield:view', 'agent_shield:manage');
DROP INDEX IF EXISTS idx_usage_logs_agent_type;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS security_events;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS agent_type;
ALTER TABLE guardrail_alert_logs DROP COLUMN IF EXISTS agent_type;
