-- 实体行为基线与自适应防护 M1（docs/plans/2026-11-19-behavior-baseline-defense.md §5.2/§7，冻结 v4.0）。
-- 四表：rollup 事实表（幂等 upsert 目标=唯一键 entity_type+entity_id+metric+bucket_ts，简单列冲突目标，
-- 不用表达式目标——DataLens U-11 的不可移植教训）、基线快照表、可移植作业锁（D15）、新颖度已见集（D13）。
-- granularity 区分小时桶（35 天保留）与日桶（180 天保留，B 类直方图）。mysql 链独立编号（与 postgres
-- 000096 / sqlite 000022 同形）。
CREATE TABLE behavior_rollups (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    entity_type  VARCHAR(16) NOT NULL,
    entity_id    BIGINT NOT NULL,
    metric       VARCHAR(128) NOT NULL,
    granularity  VARCHAR(4) NOT NULL DEFAULT 'hour',
    bucket_ts    DATETIME(3) NOT NULL,
    value_num    DOUBLE NULL,
    value_den    DOUBLE NULL,
    value_hist   TEXT NULL,
    sample_count BIGINT NOT NULL DEFAULT 0,
    updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY idx_behavior_rollups_upsert (entity_type, entity_id, metric, bucket_ts),
    INDEX idx_behavior_rollups_cleanup (granularity, bucket_ts),
    INDEX idx_behavior_rollups_entity (entity_type, entity_id, metric, bucket_ts)
);

CREATE TABLE behavior_baselines (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    entity_type   VARCHAR(16) NOT NULL,
    entity_id     BIGINT NOT NULL,
    metric        VARCHAR(128) NOT NULL,
    bucket_key    VARCHAR(8) NOT NULL,
    p50           DOUBLE NOT NULL DEFAULT 0,
    p90           DOUBLE NOT NULL DEFAULT 0,
    p99           DOUBLE NOT NULL DEFAULT 0,
    ewma          DOUBLE NOT NULL DEFAULT 0,
    ewma_var      DOUBLE NOT NULL DEFAULT 0,
    obs_count     INT NOT NULL DEFAULT 0,
    window_start  DATETIME(3) NOT NULL,
    window_end    DATETIME(3) NOT NULL,
    model_version INT NOT NULL DEFAULT 1,
    computed_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY idx_behavior_baselines_upsert (entity_type, entity_id, metric, bucket_key)
);

-- 可移植作业互斥（D15）：两步抢锁（UPDATE 过期 → INSERT 撞唯一键）+ 条件续租/释放（owner_token）。
-- fence 仅用于代际记录与诊断，不参与写入校验（明示取舍，§5.3）。
CREATE TABLE behavior_job_locks (
    job         VARCHAR(64) PRIMARY KEY,
    owner_token VARCHAR(64) NOT NULL,
    fence       BIGINT NOT NULL DEFAULT 1,
    lease_until DATETIME(3) NOT NULL
);

-- 新颖度已见集（§5.10）：180 天保留，不随 top-32 淘汰；聚合作业每小时 upsert 刷新 last_seen_at。
CREATE TABLE behavior_seen_categories (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    entity_type   VARCHAR(16) NOT NULL,
    entity_id     BIGINT NOT NULL,
    kind          VARCHAR(16) NOT NULL,
    category      VARCHAR(128) NOT NULL,
    first_seen_at DATETIME(3) NOT NULL,
    last_seen_at  DATETIME(3) NOT NULL,
    UNIQUE KEY idx_behavior_seen_upsert (entity_type, entity_id, kind, category),
    INDEX idx_behavior_seen_gc (last_seen_at)
);
