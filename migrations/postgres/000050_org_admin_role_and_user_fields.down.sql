DELETE FROM role_permissions WHERE role_id = (SELECT id FROM roles WHERE name = 'org_admin');
DELETE FROM roles WHERE name = 'org_admin';
ALTER TABLE users DROP COLUMN IF EXISTS force_password_change;
ALTER TABLE users DROP COLUMN IF EXISTS email;
