-- 行为基线防护 M2（规划冻结 v4.0 §5.9/§7）。sqlite 链独立编号；与 mysql 000020 /
-- postgres 000097 同形。
CREATE TABLE behavior_anomalies (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    event_fingerprint      TEXT NOT NULL,
    merge_bucket           INTEGER NOT NULL,
    entity_type            TEXT NOT NULL,
    entity_id              INTEGER NOT NULL,
    org_id                 INTEGER,
    score                  REAL NOT NULL DEFAULT 0,
    score_peak             REAL NOT NULL DEFAULT 0,
    band                   TEXT NOT NULL DEFAULT 'suspicious',
    dimensions             TEXT NULL,
    baseline_model_version INTEGER NOT NULL DEFAULT 1,
    first_seen_at          DATETIME NOT NULL,
    last_hit_at            DATETIME NOT NULL,
    hit_count              INTEGER NOT NULL DEFAULT 1,
    window_start           DATETIME NOT NULL,
    window_end             DATETIME NOT NULL,
    prev_event_id          INTEGER,
    status                 TEXT NOT NULL DEFAULT 'open',
    closed_reason          TEXT,
    decided_by             TEXT,
    created_at             DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at             DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_behavior_anomalies_dedup ON behavior_anomalies (event_fingerprint, merge_bucket);
CREATE INDEX idx_behavior_anomalies_sweep ON behavior_anomalies (status, last_hit_at);
CREATE INDEX idx_behavior_anomalies_entity ON behavior_anomalies (entity_type, entity_id, status);

CREATE TABLE behavior_tolerances (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type      TEXT NOT NULL,
    entity_id        INTEGER NOT NULL,
    metric           TEXT NOT NULL,
    multiplier       REAL NOT NULL DEFAULT 1,
    last_feedback_at DATETIME NOT NULL,
    created_at       DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at       DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_behavior_tolerances_upsert ON behavior_tolerances (entity_type, entity_id, metric);
