-- Sync role permissions: add newer actions to system roles
-- Admin gets role:* (syncAdminPermissions in Go handles all other new actions)
INSERT INTO role_permissions (role_id, action)
SELECT r.id, a.action FROM roles r
CROSS JOIN (VALUES
    ('role:list'), ('role:create'), ('role:update'), ('role:delete')
) AS a(action)
WHERE r.name = 'admin'
AND NOT EXISTS (
    SELECT 1 FROM role_permissions rp
    WHERE rp.role_id = r.id AND rp.action = a.action
);

-- Member: add community-tier actions added after initial RBAC migration
INSERT INTO role_permissions (role_id, action)
SELECT r.id, a.action FROM roles r
CROSS JOIN (VALUES
    ('system:password'),
    ('license:view'),
    ('mcp:list'),
    ('mcp:view')
) AS a(action)
WHERE r.name = 'member'
AND NOT EXISTS (
    SELECT 1 FROM role_permissions rp
    WHERE rp.role_id = r.id AND rp.action = a.action
);

-- Viewer: add community-tier read-only actions added after initial RBAC migration
INSERT INTO role_permissions (role_id, action)
SELECT r.id, a.action FROM roles r
CROSS JOIN (VALUES
    ('license:view'),
    ('mcp:list'),
    ('mcp:view')
) AS a(action)
WHERE r.name = 'viewer'
AND NOT EXISTS (
    SELECT 1 FROM role_permissions rp
    WHERE rp.role_id = r.id AND rp.action = a.action
);
