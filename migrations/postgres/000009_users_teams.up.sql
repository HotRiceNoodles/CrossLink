-- Users table
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    username VARCHAR(64) UNIQUE NOT NULL,
    password_hash VARCHAR(128) NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    role VARCHAR(16) NOT NULL DEFAULT 'member',
    status SMALLINT NOT NULL DEFAULT 1,
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Teams table
CREATE TABLE teams (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(128) UNIQUE NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    description TEXT,
    budget_limit DECIMAL(12,2) NOT NULL DEFAULT 0,
    budget_period VARCHAR(16) NOT NULL DEFAULT 'monthly',
    rpm_limit INT NOT NULL DEFAULT 0,
    tpm_limit INT NOT NULL DEFAULT 0,
    status SMALLINT NOT NULL DEFAULT 1,
    created_by_id BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Team members table
CREATE TABLE team_members (
    id BIGSERIAL PRIMARY KEY,
    team_id BIGINT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role VARCHAR(16) NOT NULL DEFAULT 'member',
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (team_id, user_id)
);

CREATE INDEX idx_team_members_user_id ON team_members(user_id);

-- Add user_id and team_id to api_keys
ALTER TABLE api_keys ADD COLUMN created_by_id BIGINT REFERENCES users(id);
ALTER TABLE api_keys ADD COLUMN team_id BIGINT REFERENCES teams(id);

-- Add team_id to usage_logs
ALTER TABLE usage_logs ADD COLUMN team_id BIGINT REFERENCES teams(id);

-- Migrate existing admin user from system_settings.
-- If no admin_password_hash setting exists (fresh install), this INSERT is a no-op.
-- The admin user will be created by the application's ensureAdminUser() function instead.
INSERT INTO users (username, password_hash, display_name, role)
SELECT 'admin', value, 'Administrator', 'admin'
FROM system_settings
WHERE key = 'admin_password_hash';

-- Set api_keys.created_by_id to admin user
UPDATE api_keys SET created_by_id = (SELECT id FROM users WHERE username = 'admin' LIMIT 1)
WHERE created_by_id IS NULL;
