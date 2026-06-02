CREATE INDEX IF NOT EXISTS idx_alert_logs_created_at ON guardrail_alert_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_alert_logs_severity ON guardrail_alert_logs(severity);
CREATE INDEX IF NOT EXISTS idx_alert_logs_action ON guardrail_alert_logs(action);
