-- 实体行为基线与自适应防护 M1（docs/plans/2026-11-19-behavior-baseline-defense.md §5.2/§7，冻结 v4.0）。
-- sqlite 链独立编号；与 mysql 000019 / postgres 000096 同形。SQLite 的 ON CONFLICT (列) DO UPDATE
-- 支持简单列冲突目标，幂等 upsert 与 PG 同式（经 internal/behavior 方言分支）。
CREATE TABLE behavior_rollups (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type  TEXT NOT NULL,
    entity_id    INTEGER NOT NULL,
    metric       TEXT NOT NULL,
    granularity  TEXT NOT NULL DEFAULT 'hour',
    bucket_ts    DATETIME NOT NULL,
    value_num    REAL NULL,
    value_den    REAL NULL,
    value_hist   TEXT NULL,
    sample_count INTEGER NOT NULL DEFAULT 0,
    updated_at   DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_behavior_rollups_upsert ON behavior_rollups (entity_type, entity_id, metric, bucket_ts);
CREATE INDEX idx_behavior_rollups_cleanup ON behavior_rollups (granularity, bucket_ts);
CREATE INDEX idx_behavior_rollups_entity ON behavior_rollups (entity_type, entity_id, metric, bucket_ts);

CREATE TABLE behavior_baselines (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type   TEXT NOT NULL,
    entity_id     INTEGER NOT NULL,
    metric        TEXT NOT NULL,
    bucket_key    TEXT NOT NULL,
    p50           REAL NOT NULL DEFAULT 0,
    p90           REAL NOT NULL DEFAULT 0,
    p99           REAL NOT NULL DEFAULT 0,
    ewma          REAL NOT NULL DEFAULT 0,
    ewma_var      REAL NOT NULL DEFAULT 0,
    obs_count     INTEGER NOT NULL DEFAULT 0,
    window_start  DATETIME NOT NULL,
    window_end    DATETIME NOT NULL,
    model_version INTEGER NOT NULL DEFAULT 1,
    computed_at   DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_behavior_baselines_upsert ON behavior_baselines (entity_type, entity_id, metric, bucket_key);

CREATE TABLE behavior_job_locks (
    job         TEXT PRIMARY KEY,
    owner_token TEXT NOT NULL,
    fence       INTEGER NOT NULL DEFAULT 1,
    lease_until DATETIME NOT NULL
);

CREATE TABLE behavior_seen_categories (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type   TEXT NOT NULL,
    entity_id     INTEGER NOT NULL,
    kind          TEXT NOT NULL,
    category      TEXT NOT NULL,
    first_seen_at DATETIME NOT NULL,
    last_seen_at  DATETIME NOT NULL
);
CREATE UNIQUE INDEX idx_behavior_seen_upsert ON behavior_seen_categories (entity_type, entity_id, kind, category);
CREATE INDEX idx_behavior_seen_gc ON behavior_seen_categories (last_seen_at);
