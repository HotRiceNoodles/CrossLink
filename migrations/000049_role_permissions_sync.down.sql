-- Remove role:* from admin
DELETE FROM role_permissions
WHERE role_id = (SELECT id FROM roles WHERE name = 'admin')
AND action IN ('role:list', 'role:create', 'role:update', 'role:delete');

-- Remove synced member permissions
DELETE FROM role_permissions
WHERE role_id = (SELECT id FROM roles WHERE name = 'member')
AND action IN ('system:password', 'license:view', 'mcp:list', 'mcp:view');

-- Remove synced viewer permissions
DELETE FROM role_permissions
WHERE role_id = (SELECT id FROM roles WHERE name = 'viewer')
AND action IN ('license:view', 'mcp:list', 'mcp:view');
