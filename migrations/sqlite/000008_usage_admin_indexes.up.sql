-- U-08/U-12: admin-panel composite indexes (SQLite deployments are
-- small-scale; index build cost is trivial).
CREATE INDEX IF NOT EXISTS idx_usage_logs_org_created ON usage_logs (org_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_logs_team_created ON usage_logs (team_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_logs_model_created ON usage_logs (model_requested, created_at);
CREATE INDEX IF NOT EXISTS idx_guardrail_alert_logs_org_created ON guardrail_alert_logs (org_id, created_at);
