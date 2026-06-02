-- Revert configurable RBAC
ALTER TABLE users ADD COLUMN role VARCHAR(16) NOT NULL DEFAULT 'member';
UPDATE users SET role = (SELECT name FROM roles WHERE roles.id = users.role_id);
DROP INDEX IF EXISTS idx_users_role_id;
ALTER TABLE users DROP COLUMN role_id;

DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS roles;
