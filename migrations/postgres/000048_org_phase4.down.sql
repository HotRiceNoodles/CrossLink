-- Reverse Phase 4: Remove org_id from Analytics, Insights, Budget Workflow, MCP tables

DROP INDEX IF EXISTS idx_mcp_logs_org_id;
DROP INDEX IF EXISTS idx_mcp_servers_org_id;
DROP INDEX IF EXISTS idx_budget_snapshots_org_id;
DROP INDEX IF EXISTS idx_budget_requests_org_id;
DROP INDEX IF EXISTS idx_budget_recommendations_org_id;
DROP INDEX IF EXISTS idx_optimization_actions_org_id;
DROP INDEX IF EXISTS idx_insights_org_id;

ALTER TABLE mcp_tool_call_logs DROP COLUMN IF EXISTS org_id;
ALTER TABLE mcp_servers DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_snapshots DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_requests DROP COLUMN IF EXISTS org_id;
ALTER TABLE budget_recommendations DROP COLUMN IF EXISTS org_id;
ALTER TABLE optimization_actions DROP COLUMN IF EXISTS org_id;
ALTER TABLE insights DROP COLUMN IF EXISTS org_id;
