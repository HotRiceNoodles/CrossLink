-- Add playground:use permission to admin and member roles
INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'playground:use'
FROM roles r
WHERE r.name IN ('admin', 'member')
ON CONFLICT DO NOTHING;
