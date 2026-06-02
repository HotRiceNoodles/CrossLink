-- Revert: drop and recreate soft-delete indexes (matches original migration 036).
DROP INDEX IF EXISTS idx_providers_deleted_at;
DROP INDEX IF EXISTS idx_provider_models_deleted_at;
DROP INDEX IF EXISTS idx_api_keys_deleted_at;
DROP INDEX IF EXISTS idx_users_deleted_at;
DROP INDEX IF EXISTS idx_teams_deleted_at;
DROP INDEX IF EXISTS idx_team_members_deleted_at;
DROP INDEX IF EXISTS idx_roles_deleted_at;
DROP INDEX IF EXISTS idx_budget_alerts_deleted_at;

CREATE INDEX IF NOT EXISTS idx_providers_deleted_at ON providers(deleted_at);
CREATE INDEX IF NOT EXISTS idx_provider_models_deleted_at ON provider_models(deleted_at);
CREATE INDEX IF NOT EXISTS idx_api_keys_deleted_at ON api_keys(deleted_at);
CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users(deleted_at);
CREATE INDEX IF NOT EXISTS idx_teams_deleted_at ON teams(deleted_at);
CREATE INDEX IF NOT EXISTS idx_team_members_deleted_at ON team_members(deleted_at);
CREATE INDEX IF NOT EXISTS idx_roles_deleted_at ON roles(deleted_at);
CREATE INDEX IF NOT EXISTS idx_budget_alerts_deleted_at ON budget_alerts(deleted_at);
