-- 实体行为基线与自适应防护 M1（docs/plans/2026-11-19-behavior-baseline-defense.md §5.2/§7，冻结 v4.0）。
-- 与 mysql 000019 / sqlite 000022 同形；postgres 链独立编号。upsert 目标为简单列唯一键（D9），
-- 聚合 SQL 由 Go 侧分桶（§5.7），本表不含任何表达式索引。
CREATE TABLE behavior_rollups (
    id           BIGSERIAL PRIMARY KEY,
    entity_type  VARCHAR(16) NOT NULL,
    entity_id    BIGINT NOT NULL,
    metric       VARCHAR(128) NOT NULL,
    granularity  VARCHAR(4) NOT NULL DEFAULT 'hour',
    bucket_ts    TIMESTAMPTZ NOT NULL,
    value_num    DOUBLE PRECISION NULL,
    value_den    DOUBLE PRECISION NULL,
    value_hist   TEXT NULL,
    sample_count BIGINT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idx_behavior_rollups_upsert UNIQUE (entity_type, entity_id, metric, bucket_ts)
);
CREATE INDEX idx_behavior_rollups_cleanup ON behavior_rollups (granularity, bucket_ts);
CREATE INDEX idx_behavior_rollups_entity ON behavior_rollups (entity_type, entity_id, metric, bucket_ts);

CREATE TABLE behavior_baselines (
    id            BIGSERIAL PRIMARY KEY,
    entity_type   VARCHAR(16) NOT NULL,
    entity_id     BIGINT NOT NULL,
    metric        VARCHAR(128) NOT NULL,
    bucket_key    VARCHAR(8) NOT NULL,
    p50           DOUBLE PRECISION NOT NULL DEFAULT 0,
    p90           DOUBLE PRECISION NOT NULL DEFAULT 0,
    p99           DOUBLE PRECISION NOT NULL DEFAULT 0,
    ewma          DOUBLE PRECISION NOT NULL DEFAULT 0,
    ewma_var      DOUBLE PRECISION NOT NULL DEFAULT 0,
    obs_count     INT NOT NULL DEFAULT 0,
    window_start  TIMESTAMPTZ NOT NULL,
    window_end    TIMESTAMPTZ NOT NULL,
    model_version INT NOT NULL DEFAULT 1,
    computed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idx_behavior_baselines_upsert UNIQUE (entity_type, entity_id, metric, bucket_key)
);

-- 可移植作业互斥（D15）：两步抢锁 + 条件续租/释放（owner_token）；fence 仅代际记录。
CREATE TABLE behavior_job_locks (
    job         VARCHAR(64) PRIMARY KEY,
    owner_token VARCHAR(64) NOT NULL,
    fence       BIGINT NOT NULL DEFAULT 1,
    lease_until TIMESTAMPTZ NOT NULL
);

-- 新颖度已见集（§5.10）：180 天保留，不随 top-32 淘汰。
CREATE TABLE behavior_seen_categories (
    id            BIGSERIAL PRIMARY KEY,
    entity_type   VARCHAR(16) NOT NULL,
    entity_id     BIGINT NOT NULL,
    kind          VARCHAR(16) NOT NULL,
    category      VARCHAR(128) NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    CONSTRAINT idx_behavior_seen_upsert UNIQUE (entity_type, entity_id, kind, category)
);
CREATE INDEX idx_behavior_seen_gc ON behavior_seen_categories (last_seen_at);
