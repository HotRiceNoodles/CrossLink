-- Phase 4: Add org_id to Analytics, Insights, Budget Workflow, MCP tables
-- Uses IF NOT EXISTS to be idempotent if migration is re-run.

-- 1. Add nullable org_id columns (no inline FK — checked by app logic)
ALTER TABLE insights ADD COLUMN IF NOT EXISTS org_id BIGINT;
ALTER TABLE optimization_actions ADD COLUMN IF NOT EXISTS org_id BIGINT;
ALTER TABLE budget_recommendations ADD COLUMN IF NOT EXISTS org_id BIGINT;
ALTER TABLE budget_requests ADD COLUMN IF NOT EXISTS org_id BIGINT;
ALTER TABLE budget_snapshots ADD COLUMN IF NOT EXISTS org_id BIGINT;
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS org_id BIGINT;
-- Partition table: ALTER on parent cascades to all partitions
ALTER TABLE mcp_tool_call_logs ADD COLUMN IF NOT EXISTS org_id BIGINT;

-- 2. Backfill small tables to default org
UPDATE insights SET org_id = 1 WHERE org_id IS NULL;
UPDATE optimization_actions SET org_id = 1 WHERE org_id IS NULL;
UPDATE budget_recommendations SET org_id = 1 WHERE org_id IS NULL;
UPDATE budget_requests SET org_id = 1 WHERE org_id IS NULL;
UPDATE budget_snapshots SET org_id = 1 WHERE org_id IS NULL;
UPDATE mcp_servers SET org_id = 1 WHERE org_id IS NULL AND deleted_at IS NULL;

-- 3. Backfill mcp_tool_call_logs via server ownership
UPDATE mcp_tool_call_logs l
SET org_id = s.org_id
FROM mcp_servers s
WHERE l.server_id = s.id AND l.org_id IS NULL;

-- Remaining mcp_tool_call_logs without a matching server get default org
UPDATE mcp_tool_call_logs SET org_id = 1 WHERE org_id IS NULL;

-- 4. Create indexes (after backfill for performance)
CREATE INDEX IF NOT EXISTS idx_insights_org_id ON insights(org_id);
CREATE INDEX IF NOT EXISTS idx_optimization_actions_org_id ON optimization_actions(org_id);
CREATE INDEX IF NOT EXISTS idx_budget_recommendations_org_id ON budget_recommendations(org_id);
CREATE INDEX IF NOT EXISTS idx_budget_requests_org_id ON budget_requests(org_id);
CREATE INDEX IF NOT EXISTS idx_budget_snapshots_org_id ON budget_snapshots(org_id);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_org_id ON mcp_servers(org_id);
-- Index on partition table parent cascades to partitions
CREATE INDEX IF NOT EXISTS idx_mcp_logs_org_id ON mcp_tool_call_logs(org_id);
