-- ============================================================
-- IRREVERSIBLE MIGRATION WARNING
-- ============================================================
-- This migration converts TIMESTAMP columns to TIMESTAMPTZ,
-- interpreting existing values as Asia/Shanghai local time.
--
-- If your database operates in a DIFFERENT timezone, the
-- converted timestamps will be INCORRECT by the offset
-- difference between your timezone and Asia/Shanghai (UTC+8).
--
-- Before running this migration in non-China deployments:
-- 1. Check your PostgreSQL timezone: SHOW timezone;
-- 2. If NOT 'Asia/Shanghai' or 'PRC', replace all instances
--    of 'Asia/Shanghai' below with your actual timezone.
-- 3. Consider taking a backup before running.
-- ============================================================

ALTER TABLE guardrail_alert_rules
  ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'Asia/Shanghai',
  ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING updated_at AT TIME ZONE 'Asia/Shanghai',
  ALTER COLUMN last_triggered_at TYPE TIMESTAMPTZ USING last_triggered_at AT TIME ZONE 'Asia/Shanghai';

ALTER TABLE guardrail_alert_logs
  ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'Asia/Shanghai';
