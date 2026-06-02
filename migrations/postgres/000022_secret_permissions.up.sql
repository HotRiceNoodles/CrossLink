-- Add secret management permissions for admin role
INSERT INTO role_permissions (role_id, action)
SELECT id, 'secret:test' FROM roles WHERE name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action)
SELECT id, 'secret:manage' FROM roles WHERE name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;
