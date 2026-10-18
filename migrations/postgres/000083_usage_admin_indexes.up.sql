-- U-08/U-12: admin-panel composite indexes. Every panel query filters
-- (org_id|team_id) AND created_at; single-column indexes meant a scan+filter
-- over the whole org's retention. Plain CREATE INDEX matches the migration
-- set's established form; on very large production tables run this file's
-- statements manually with CONCURRENTLY during a maintenance window instead.
CREATE INDEX IF NOT EXISTS idx_usage_logs_org_created ON usage_logs (org_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_logs_team_created ON usage_logs (team_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_logs_model_created ON usage_logs (model_requested, created_at);
CREATE INDEX IF NOT EXISTS idx_guardrail_alert_logs_org_created ON guardrail_alert_logs (org_id, created_at);
