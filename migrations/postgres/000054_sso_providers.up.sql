-- SSO extension: provider table + users table SSO fields + partial unique indexes
-- Note: includes both SSO-specific changes and global improvements (email/username partial indexes)

-- 1. Create SSO providers table
CREATE TABLE IF NOT EXISTS sso_providers (
    id                BIGSERIAL PRIMARY KEY,
    name              VARCHAR(64) NOT NULL,
    type              VARCHAR(16) NOT NULL DEFAULT 'oidc',
    issuer            VARCHAR(512) NOT NULL,
    client_id         VARCHAR(256) NOT NULL,
    client_secret     TEXT NOT NULL,
    scopes            VARCHAR(256) DEFAULT 'openid profile email',
    default_org_id    BIGINT REFERENCES organizations(id),
    default_role_id   BIGINT REFERENCES roles(id),
    auto_create       BOOLEAN NOT NULL DEFAULT true,
    redirect_base_url VARCHAR(512),
    auth_url          VARCHAR(512),
    token_url         VARCHAR(512),
    userinfo_url      VARCHAR(512),
    enabled           BOOLEAN NOT NULL DEFAULT true,
    created_at        TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMP NOT NULL DEFAULT NOW()
);

-- 2. Add SSO identity columns to users
ALTER TABLE users ADD COLUMN IF NOT EXISTS sso_id VARCHAR(256) DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS sso_provider_id BIGINT;

-- 3. Partial unique index: SSO identity (one IdP user → one local user)
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_sso
    ON users(sso_provider_id, sso_id)
    WHERE sso_id IS NOT NULL AND sso_id != '' AND deleted_at IS NULL;

-- 4. Email partial unique index (SSO email merge prerequisite)
-- ⚠️ Pre-check: if this query returns rows, clean up duplicates before running:
--   SELECT email, COUNT(*) FROM users WHERE email IS NOT NULL AND email != '' AND deleted_at IS NULL
--     GROUP BY email HAVING COUNT(*) > 1;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_active_idx
    ON users(email)
    WHERE email IS NOT NULL AND email != '' AND deleted_at IS NULL;

-- 5. Replace username full-table unique with partial unique index
-- (allows soft-deleted rows to coexist with new rows having same username)
-- Must drop constraint first (UNIQUE constraint creates both constraint + index)
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_username_key;
DROP INDEX IF EXISTS users_username_key;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_active_idx
    ON users(username)
    WHERE deleted_at IS NULL;
