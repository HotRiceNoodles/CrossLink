DROP INDEX IF EXISTS idx_budget_alerts_deleted_at;
DROP INDEX IF EXISTS idx_roles_deleted_at;
DROP INDEX IF EXISTS idx_team_members_deleted_at;
DROP INDEX IF EXISTS idx_teams_deleted_at;
DROP INDEX IF EXISTS idx_users_deleted_at;
DROP INDEX IF EXISTS idx_api_keys_deleted_at;
DROP INDEX IF EXISTS idx_provider_models_deleted_at;
DROP INDEX IF EXISTS idx_providers_deleted_at;

ALTER TABLE budget_alerts DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE roles DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE team_members DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE teams DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE users DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE api_keys DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE provider_models DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE providers DROP COLUMN IF EXISTS deleted_at;
