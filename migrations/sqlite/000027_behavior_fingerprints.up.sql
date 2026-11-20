-- 行为指纹候选库(M3 反馈闭环,冻结 v4.0 §5.6/D10):confirmed 异常产出候选
-- (命中维度组合+取值模板),人在环复核——审批不自动生效(observe-only 纪律)。
-- sqlite 链:清越社区(26)与 overlay(24)链尾。
CREATE TABLE behavior_fingerprints (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type    TEXT NOT NULL,
    entity_id      INTEGER NOT NULL,
    dimension_set  TEXT NOT NULL,
    template       TEXT NOT NULL,
    source_event_id INTEGER,
    hit_count      INTEGER NOT NULL DEFAULT 1,
    status         TEXT NOT NULL DEFAULT 'candidate',
    decided_by     TEXT,
    created_at     DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at     DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_behavior_fps_dedupe ON behavior_fingerprints (entity_type, entity_id, dimension_set);
CREATE INDEX idx_behavior_fps_status ON behavior_fingerprints (status);
