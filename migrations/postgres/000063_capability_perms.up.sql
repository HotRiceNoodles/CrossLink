-- Grant capability admin actions to the admin (super-admin) role
INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'capability:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'capability:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'capability:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'capability:delete')
ON CONFLICT DO NOTHING;
