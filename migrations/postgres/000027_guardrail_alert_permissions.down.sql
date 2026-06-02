DELETE FROM role_permissions WHERE action IN (
    'guardrail_alert:list', 'guardrail_alert:create',
    'guardrail_alert:update', 'guardrail_alert:delete', 'guardrail_alert:logs'
);
