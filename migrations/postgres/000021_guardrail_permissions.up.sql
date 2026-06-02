-- Add guardrail permissions to admin and member roles
INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'guardrail:test')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'member'), 'guardrail:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'guardrail:test')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'viewer'), 'guardrail:list')
ON CONFLICT (role_id, action) DO NOTHING;
