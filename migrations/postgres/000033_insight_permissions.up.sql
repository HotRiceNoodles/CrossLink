-- Add insight:manage permission to admin and member roles
INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'insight:manage'
FROM roles r
WHERE r.name IN ('admin', 'member')
ON CONFLICT (role_id, action) DO NOTHING;
