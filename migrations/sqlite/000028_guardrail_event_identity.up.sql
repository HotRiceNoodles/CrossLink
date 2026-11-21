-- 护栏自适应调参 M1(规划 v1.7 §6):事件身份 + 结构化分数 + 仲裁列(见 mysql/000025)。
-- sqlite 链:社区(27)与 overlay(24)链尾之上取 28。SQLite ADD COLUMN 一次一列。
ALTER TABLE guardrail_alert_logs ADD COLUMN event_id TEXT NULL;
ALTER TABLE guardrail_alert_logs ADD COLUMN scores TEXT NULL;
ALTER TABLE guardrail_alert_logs ADD COLUMN decision TEXT NULL;
ALTER TABLE guardrail_alert_logs ADD COLUMN decided_by INTEGER NULL;
ALTER TABLE guardrail_alert_logs ADD COLUMN decided_by_name TEXT NULL;
ALTER TABLE guardrail_alert_logs ADD COLUMN decided_at DATETIME NULL;
ALTER TABLE guardrail_alert_logs ADD COLUMN decision_note TEXT NULL;
CREATE INDEX IF NOT EXISTS idx_guardrail_alert_logs_event ON guardrail_alert_logs(event_id);

ALTER TABLE compliance_incidents ADD COLUMN event_id TEXT NULL;
CREATE INDEX IF NOT EXISTS idx_compliance_incidents_event ON compliance_incidents(event_id);
