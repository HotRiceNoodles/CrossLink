DROP INDEX idx_guardrail_alert_logs_event ON guardrail_alert_logs;
ALTER TABLE guardrail_alert_logs
    DROP COLUMN event_id,
    DROP COLUMN scores,
    DROP COLUMN decision,
    DROP COLUMN decided_by,
    DROP COLUMN decided_by_name,
    DROP COLUMN decided_at,
    DROP COLUMN decision_note;

DROP INDEX idx_compliance_incidents_event ON compliance_incidents;
ALTER TABLE compliance_incidents
    DROP COLUMN event_id;
