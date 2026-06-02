ALTER TABLE usage_logs ADD COLUMN guardrail_triggered BOOLEAN DEFAULT false;
ALTER TABLE usage_logs ADD COLUMN guardrail_rule VARCHAR(255);
CREATE INDEX idx_usage_logs_guardrail ON usage_logs(guardrail_triggered) WHERE guardrail_triggered = true;
