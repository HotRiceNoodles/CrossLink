-- Grant routing stats action to the admin (super-admin) role
INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'routing:stats')
ON CONFLICT DO NOTHING;
