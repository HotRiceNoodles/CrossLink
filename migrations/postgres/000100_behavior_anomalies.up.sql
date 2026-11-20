-- 行为基线防护 M2（规划冻结 v4.0 §5.9/§7）。与 mysql 000020 / sqlite 000023 同形；
-- postgres 链独立编号。
CREATE TABLE behavior_anomalies (
    id                     BIGSERIAL PRIMARY KEY,
    event_fingerprint      VARCHAR(64) NOT NULL,
    merge_bucket           BIGINT NOT NULL,
    entity_type            VARCHAR(16) NOT NULL,
    entity_id              BIGINT NOT NULL,
    org_id                 BIGINT NULL,
    score                  DOUBLE PRECISION NOT NULL DEFAULT 0,
    score_peak             DOUBLE PRECISION NOT NULL DEFAULT 0,
    band                   VARCHAR(16) NOT NULL DEFAULT 'suspicious',
    dimensions             TEXT NULL,
    baseline_model_version INT NOT NULL DEFAULT 1,
    first_seen_at          TIMESTAMPTZ NOT NULL,
    last_hit_at            TIMESTAMPTZ NOT NULL,
    hit_count              INT NOT NULL DEFAULT 1,
    window_start           TIMESTAMPTZ NOT NULL,
    window_end             TIMESTAMPTZ NOT NULL,
    prev_event_id          BIGINT NULL,
    status                 VARCHAR(16) NOT NULL DEFAULT 'open',
    closed_reason          VARCHAR(64) NULL,
    decided_by             VARCHAR(64) NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idx_behavior_anomalies_dedup UNIQUE (event_fingerprint, merge_bucket)
);
CREATE INDEX idx_behavior_anomalies_sweep ON behavior_anomalies (status, last_hit_at);
CREATE INDEX idx_behavior_anomalies_entity ON behavior_anomalies (entity_type, entity_id, status);

CREATE TABLE behavior_tolerances (
    id               BIGSERIAL PRIMARY KEY,
    entity_type      VARCHAR(16) NOT NULL,
    entity_id        BIGINT NOT NULL,
    metric           VARCHAR(128) NOT NULL,
    multiplier       DOUBLE PRECISION NOT NULL DEFAULT 1,
    last_feedback_at TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idx_behavior_tolerances_upsert UNIQUE (entity_type, entity_id, metric)
);
