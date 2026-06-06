-- Rollback SSO extension

-- Reverse: drop new partial indexes first
DROP INDEX IF EXISTS users_username_active_idx;
DROP INDEX IF EXISTS users_email_active_idx;
DROP INDEX IF EXISTS idx_users_sso;

-- Recreate original username full-table unique index
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key ON users(username);

-- Reverse: drop SSO columns from users
ALTER TABLE users DROP COLUMN IF EXISTS sso_provider_id;
ALTER TABLE users DROP COLUMN IF EXISTS sso_id;

-- Reverse: drop SSO providers table
DROP TABLE IF EXISTS sso_providers;
