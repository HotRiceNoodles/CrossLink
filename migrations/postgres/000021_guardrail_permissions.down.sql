DELETE FROM role_permissions WHERE action IN ('guardrail:list', 'guardrail:create', 'guardrail:update', 'guardrail:delete', 'guardrail:test');
