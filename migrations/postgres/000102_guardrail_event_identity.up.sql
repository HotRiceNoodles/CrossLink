-- 护栏自适应调参 M1(规划 v1.7 §6):事件身份 + 结构化分数 + 仲裁列(见 mysql/000025)。
-- postgres 链:社区(101)与 overlay(98)链尾之上取 102。
ALTER TABLE guardrail_alert_logs
    ADD COLUMN IF NOT EXISTS event_id VARCHAR(36),
    ADD COLUMN IF NOT EXISTS scores JSONB,
    ADD COLUMN IF NOT EXISTS decision VARCHAR(16),
    ADD COLUMN IF NOT EXISTS decided_by BIGINT,
    ADD COLUMN IF NOT EXISTS decided_by_name VARCHAR(64),
    ADD COLUMN IF NOT EXISTS decided_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS decision_note VARCHAR(500);
CREATE INDEX IF NOT EXISTS idx_guardrail_alert_logs_event ON guardrail_alert_logs(event_id);

ALTER TABLE compliance_incidents
    ADD COLUMN IF NOT EXISTS event_id VARCHAR(36);
CREATE INDEX IF NOT EXISTS idx_compliance_incidents_event ON compliance_incidents(event_id);
