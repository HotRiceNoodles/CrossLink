-- ============================================================
-- Migration 000047 (down): Drop org_id columns, indexes, tables
-- ============================================================

-- 1. Drop indexes
DROP INDEX IF EXISTS idx_teams_org_id;
DROP INDEX IF EXISTS idx_api_keys_org_id;
DROP INDEX IF EXISTS idx_providers_org_id;
DROP INDEX IF EXISTS idx_usage_logs_org_id;
DROP INDEX IF EXISTS idx_audit_logs_org_id;
DROP INDEX IF EXISTS idx_org_members_org_id;
DROP INDEX IF EXISTS idx_org_members_user_id;

-- 2. Restore budget_alerts CHECK constraint to two-way (team_id XOR key_id)
ALTER TABLE budget_alerts DROP CONSTRAINT chk_alert_target;
ALTER TABLE budget_alerts ADD CONSTRAINT chk_alert_target CHECK ((team_id IS NULL) <> (key_id IS NULL));

-- 3. Drop org_id columns from all tables
ALTER TABLE teams DROP COLUMN IF EXISTS org_id;
ALTER TABLE api_keys DROP COLUMN IF EXISTS org_id;
ALTER TABLE providers DROP COLUMN IF EXISTS org_id;
ALTER TABLE roles DROP COLUMN IF EXISTS org_id;
ALTER TABLE users DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_alerts DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_snapshots DROP COLUMN IF EXISTS org_id;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS org_id;

-- Commercial feature tables
ALTER TABLE audit_logs DROP COLUMN IF EXISTS org_id;
ALTER TABLE insights DROP COLUMN IF EXISTS org_id;
ALTER TABLE optimization_actions DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_recommendations DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_requests DROP COLUMN IF EXISTS org_id;
ALTER TABLE agent_fingerprints DROP COLUMN IF EXISTS org_id;
ALTER TABLE guardrail_rules DROP COLUMN IF EXISTS org_id;
ALTER TABLE guardrail_alert_rules DROP COLUMN IF EXISTS org_id;
ALTER TABLE guardrail_alert_logs DROP COLUMN IF EXISTS org_id;

-- 4. Drop tables (members first due to FK reference)
DROP TABLE IF EXISTS organization_members;
DROP TABLE IF EXISTS organizations;
