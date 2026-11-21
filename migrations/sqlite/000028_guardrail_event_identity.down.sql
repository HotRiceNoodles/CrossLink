DROP INDEX IF EXISTS idx_guardrail_alert_logs_event;
ALTER TABLE guardrail_alert_logs DROP COLUMN event_id;
ALTER TABLE guardrail_alert_logs DROP COLUMN scores;
ALTER TABLE guardrail_alert_logs DROP COLUMN decision;
ALTER TABLE guardrail_alert_logs DROP COLUMN decided_by;
ALTER TABLE guardrail_alert_logs DROP COLUMN decided_by_name;
ALTER TABLE guardrail_alert_logs DROP COLUMN decided_at;
ALTER TABLE guardrail_alert_logs DROP COLUMN decision_note;

DROP INDEX IF EXISTS idx_compliance_incidents_event;
ALTER TABLE compliance_incidents DROP COLUMN event_id;
