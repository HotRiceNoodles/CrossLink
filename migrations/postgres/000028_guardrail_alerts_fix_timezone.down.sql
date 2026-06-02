ALTER TABLE guardrail_alert_rules
  ALTER COLUMN created_at TYPE TIMESTAMP USING created_at AT TIME ZONE 'Asia/Shanghai',
  ALTER COLUMN updated_at TYPE TIMESTAMP USING updated_at AT TIME ZONE 'Asia/Shanghai',
  ALTER COLUMN last_triggered_at TYPE TIMESTAMP USING last_triggered_at AT TIME ZONE 'Asia/Shanghai';

ALTER TABLE guardrail_alert_logs
  ALTER COLUMN created_at TYPE TIMESTAMP USING created_at AT TIME ZONE 'Asia/Shanghai';
