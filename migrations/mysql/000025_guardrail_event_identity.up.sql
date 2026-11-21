-- 护栏自适应调参 M1(规划 v1.7 §6):事件身份 + 结构化分数 + 仲裁列。
-- event_id 由 GuardrailService.emitEvent 统一赋值(UUIDv7),告警检测日志与
-- compliance_incidents 共用同一 id——窗口统计按它去重/回写,替代时间戳+摘要启发式。
-- 存量行为 NULL,不参与去重统计(28 天滚动窗口天然收敛)。
-- mysql 链:社区(24)与 overlay(21)链尾之上取 25。
ALTER TABLE guardrail_alert_logs
    ADD COLUMN event_id VARCHAR(36) NULL,
    ADD COLUMN scores JSON NULL,
    ADD COLUMN decision VARCHAR(16) NULL,
    ADD COLUMN decided_by BIGINT NULL,
    ADD COLUMN decided_by_name VARCHAR(64) NULL,
    ADD COLUMN decided_at DATETIME(3) NULL,
    ADD COLUMN decision_note VARCHAR(500) NULL;
CREATE INDEX idx_guardrail_alert_logs_event ON guardrail_alert_logs(event_id);

ALTER TABLE compliance_incidents
    ADD COLUMN event_id VARCHAR(36) NULL;
CREATE INDEX idx_compliance_incidents_event ON compliance_incidents(event_id);
