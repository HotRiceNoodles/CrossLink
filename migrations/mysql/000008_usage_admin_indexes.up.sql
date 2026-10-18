-- U-08/U-12: admin-panel composite indexes (MySQL InnoDB adds secondary
-- indexes ONLINE/INPLACE by default; no table rebuild).
CREATE INDEX idx_usage_logs_org_created ON usage_logs (org_id, created_at);
CREATE INDEX idx_usage_logs_team_created ON usage_logs (team_id, created_at);
CREATE INDEX idx_usage_logs_model_created ON usage_logs (model_requested, created_at);
CREATE INDEX idx_usage_logs_fallback_created ON usage_logs (fallback_count, created_at);
CREATE INDEX idx_guardrail_alert_logs_org_created ON guardrail_alert_logs (org_id, created_at);
