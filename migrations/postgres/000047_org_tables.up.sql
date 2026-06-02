-- ============================================================
-- Migration 000047: Multi-tenant Organization tables
-- Creates organizations, organization_members tables,
-- adds org_id columns to existing tables, backfills data.
-- ============================================================

-- -----------------------------------------------------------
-- 1. DDL: Create organizations table
-- -----------------------------------------------------------
CREATE TABLE organizations (
    id              BIGSERIAL PRIMARY KEY,
    name            VARCHAR(64) NOT NULL UNIQUE,
                    -- URL-safe slug, CHECK constraint enforces format
    display_name    VARCHAR(128) NOT NULL,
    description     TEXT,
    status          SMALLINT NOT NULL DEFAULT 1,   -- 1=active, 0=disabled
    budget_limit    DECIMAL(12,2) NOT NULL DEFAULT 0,
    budget_period   VARCHAR(16) NOT NULL DEFAULT 'monthly',
    rpm_limit       INTEGER NOT NULL DEFAULT 0,
    tpm_limit       INTEGER NOT NULL DEFAULT 0,
    settings        JSONB,
    created_by_id   BIGINT REFERENCES users(id),
    created_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMP,
    CONSTRAINT org_name_slug CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$')
);

-- -----------------------------------------------------------
-- 2. DDL: Create organization_members table
-- -----------------------------------------------------------
CREATE TABLE organization_members (
    id          BIGSERIAL PRIMARY KEY,
    org_id      BIGINT NOT NULL REFERENCES organizations(id),
    user_id     BIGINT NOT NULL REFERENCES users(id),
    role        VARCHAR(16) NOT NULL,  -- "owner" | "admin" | "member"
    joined_at   TIMESTAMP NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMP,
    UNIQUE(org_id, user_id)
);

CREATE INDEX idx_org_members_org_id ON organization_members(org_id);
CREATE INDEX idx_org_members_user_id ON organization_members(user_id);

-- -----------------------------------------------------------
-- 3. Insert default organization
-- -----------------------------------------------------------
INSERT INTO organizations (id, name, display_name, status)
VALUES (1, 'default', 'Default Organization', 1);

-- -----------------------------------------------------------
-- 4. Add org_id column to existing tables (all nullable initially)
-- -----------------------------------------------------------

-- Core tables
ALTER TABLE teams ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE api_keys ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE providers ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE roles ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE users ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE budget_alerts ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE budget_snapshots ADD COLUMN org_id BIGINT REFERENCES organizations(id);

-- Commercial feature tables
ALTER TABLE audit_logs ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE insights ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE optimization_actions ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE budget_recommendations ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE budget_requests ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE agent_fingerprints ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE guardrail_rules ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE guardrail_alert_rules ADD COLUMN org_id BIGINT REFERENCES organizations(id);
ALTER TABLE guardrail_alert_logs ADD COLUMN org_id BIGINT REFERENCES organizations(id);

-- Add org_id to usage_logs (nullable, backfill done in Go app code)
ALTER TABLE usage_logs ADD COLUMN org_id BIGINT REFERENCES organizations(id);

-- -----------------------------------------------------------
-- 5. Update budget_alerts CHECK constraint to three-way
--    (org_id + team_id + key_id exactly one non-NULL)
-- -----------------------------------------------------------
ALTER TABLE budget_alerts DROP CONSTRAINT IF EXISTS chk_alert_target;
ALTER TABLE budget_alerts ADD CONSTRAINT chk_alert_target CHECK (
    (org_id IS NOT NULL)::int +
    (team_id IS NOT NULL)::int +
    (key_id IS NOT NULL)::int = 1
);

-- -----------------------------------------------------------
-- 6. Backfill small tables
-- -----------------------------------------------------------

-- Core tables with soft delete
UPDATE teams SET org_id = 1 WHERE org_id IS NULL AND deleted_at IS NULL;
UPDATE api_keys SET org_id = 1 WHERE org_id IS NULL AND deleted_at IS NULL;
UPDATE providers SET org_id = 1 WHERE org_id IS NULL AND deleted_at IS NULL;

-- Roles: system roles keep org_id NULL, custom roles get org_id = 1
UPDATE roles SET org_id = NULL WHERE is_system = true AND deleted_at IS NULL;
UPDATE roles SET org_id = 1 WHERE is_system = false AND org_id IS NULL AND deleted_at IS NULL;

-- Users: Super Admins (admin role) keep org_id NULL
UPDATE users SET org_id = NULL
  WHERE role_id IN (SELECT id FROM roles WHERE name = 'admin' AND deleted_at IS NULL);
-- Remaining non-admin users get org_id = 1
UPDATE users SET org_id = 1
  WHERE org_id IS NULL AND deleted_at IS NULL
  AND id NOT IN (SELECT user_id FROM organization_members WHERE org_id = 1);

-- Budget tables
UPDATE budget_alerts SET org_id = 1 WHERE org_id IS NULL AND deleted_at IS NULL;
UPDATE budget_snapshots SET org_id = 1 WHERE org_id IS NULL;

-- Commercial feature tables (small tables, direct UPDATE)
UPDATE audit_logs SET org_id = 1 WHERE org_id IS NULL;
UPDATE insights SET org_id = 1 WHERE org_id IS NULL;
UPDATE optimization_actions SET org_id = 1 WHERE org_id IS NULL;
UPDATE budget_recommendations SET org_id = 1 WHERE org_id IS NULL;
UPDATE budget_requests SET org_id = 1 WHERE org_id IS NULL;
UPDATE agent_fingerprints SET org_id = 1 WHERE org_id IS NULL;
-- guardrail_rules has NO deleted_at column (no soft delete)
UPDATE guardrail_rules SET org_id = 1 WHERE org_id IS NULL;
UPDATE guardrail_alert_rules SET org_id = 1 WHERE org_id IS NULL;
UPDATE guardrail_alert_logs SET org_id = 1 WHERE org_id IS NULL;

-- NOTE: usage_logs backfill is NOT done here.
-- usage_logs is a large table that must be backfilled in Go application code
-- using batched updates (see design doc section 5.1.4).
-- The Go backfill function runs post-migration in ensureDefaultOrganization().

-- -----------------------------------------------------------
-- 7. Backfill organization_members from users table
-- -----------------------------------------------------------
INSERT INTO organization_members (org_id, user_id, role, joined_at)
SELECT 1, id,
  CASE WHEN role_id IN (SELECT id FROM roles WHERE name = 'admin' AND deleted_at IS NULL)
       THEN 'owner' ELSE 'member' END,
  NOW()
FROM users WHERE deleted_at IS NULL;

-- -----------------------------------------------------------
-- 8. Create indexes on org_id for high-query tables
-- -----------------------------------------------------------
CREATE INDEX idx_teams_org_id ON teams(org_id);
CREATE INDEX idx_api_keys_org_id ON api_keys(org_id);
CREATE INDEX idx_providers_org_id ON providers(org_id);
CREATE INDEX idx_usage_logs_org_id ON usage_logs(org_id);
CREATE INDEX idx_audit_logs_org_id ON audit_logs(org_id);

-- -----------------------------------------------------------
-- 9. Add NOT NULL constraint on teams.org_id
-- -----------------------------------------------------------
ALTER TABLE teams ALTER COLUMN org_id SET NOT NULL;
