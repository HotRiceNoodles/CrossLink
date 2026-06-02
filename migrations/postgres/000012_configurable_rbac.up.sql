-- Configurable RBAC: roles + role_permissions tables
CREATE TABLE roles (
    id           BIGSERIAL PRIMARY KEY,
    name         VARCHAR(32) UNIQUE NOT NULL,
    display_name VARCHAR(64) NOT NULL,
    is_system    BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE role_permissions (
    id       BIGSERIAL PRIMARY KEY,
    role_id  BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    action   VARCHAR(64) NOT NULL,
    UNIQUE(role_id, action)
);

-- Seed system roles
INSERT INTO roles (name, display_name, is_system) VALUES
    ('admin', '管理员', true),
    ('member', '成员', true),
    ('viewer', '观察者', true);

-- Seed default permissions for admin (all actions)
INSERT INTO role_permissions (role_id, action) VALUES
    -- admin gets all actions
    ((SELECT id FROM roles WHERE name = 'admin'), 'provider:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'provider:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'provider:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'provider:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'provider:test'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'model:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'model:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'model:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'model:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:regenerate'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:rotate'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'key:hashes'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'team:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'team:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'team:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'team:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'team:manage_members'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'user:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'user:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'user:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'user:delete'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'usage:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'usage:export'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'usage:stats'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'system:view'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'system:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'debug:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'debug:clear'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'budget:manage');

-- Seed default permissions for member (read + key manage + usage)
INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'member'), 'provider:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'model:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:create'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:update'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:delete'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:regenerate'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:rotate'),
    ((SELECT id FROM roles WHERE name = 'member'), 'key:hashes'),
    ((SELECT id FROM roles WHERE name = 'member'), 'team:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'team:update'),
    ((SELECT id FROM roles WHERE name = 'member'), 'team:manage_members'),
    ((SELECT id FROM roles WHERE name = 'member'), 'usage:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'usage:export'),
    ((SELECT id FROM roles WHERE name = 'member'), 'usage:stats');

-- Seed default permissions for viewer (read only)
INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'viewer'), 'provider:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'model:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'key:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'team:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'usage:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'usage:stats');

-- Migrate users.role → users.role_id
-- Coerce any unexpected role values to 'member' before migration
UPDATE users SET role = 'member' WHERE role NOT IN ('admin', 'member', 'viewer');
ALTER TABLE users ADD COLUMN role_id BIGINT REFERENCES roles(id);
UPDATE users SET role_id = (SELECT id FROM roles WHERE name = users.role);
ALTER TABLE users ALTER COLUMN role_id SET NOT NULL;
ALTER TABLE users DROP COLUMN role;
CREATE INDEX idx_users_role_id ON users(role_id);
