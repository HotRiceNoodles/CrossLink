-- Fix: migration 036 created indexes without IF NOT EXISTS on some tables,
-- and used plain CREATE INDEX. This migration standardizes all 8 soft-delete
-- indexes. Note: CONCURRENTLY is not used because golang-migrate wraps
-- migrations in transactions, and CREATE INDEX CONCURRENTLY cannot run
-- inside a transaction block. For large tables, run index recreation
-- manually with CONCURRENTLY outside of golang-migrate.

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