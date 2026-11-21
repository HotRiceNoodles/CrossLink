DROP INDEX IF EXISTS idx_guardrail_alert_logs_event;
ALTER TABLE guardrail_alert_logs
    DROP COLUMN IF EXISTS event_id,
    DROP COLUMN IF EXISTS scores,
    DROP COLUMN IF EXISTS decision,
    DROP COLUMN IF EXISTS decided_by,
    DROP COLUMN IF EXISTS decided_by_name,
    DROP COLUMN IF EXISTS decided_at,
    DROP COLUMN IF EXISTS decision_note;

DROP INDEX IF EXISTS idx_compliance_incidents_event;
ALTER TABLE compliance_incidents
    DROP COLUMN IF EXISTS event_id;
