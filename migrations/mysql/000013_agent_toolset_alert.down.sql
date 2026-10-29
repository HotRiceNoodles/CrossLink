DELETE FROM role_permissions WHERE action IN ('toolset_alert:list', 'toolset_alert:create', 'toolset_alert:update', 'toolset_alert:delete');
DROP TABLE IF EXISTS agent_toolset_alert_rules;
