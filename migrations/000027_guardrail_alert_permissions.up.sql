INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail_alert:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail_alert:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail_alert:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail_alert:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail_alert:logs')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'member'), 'guardrail_alert:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'guardrail_alert:logs')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'viewer'), 'guardrail_alert:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'guardrail_alert:logs')
ON CONFLICT (role_id, action) DO NOTHING;
