-- 行为基线防护 M2（规划冻结 v4.0 §5.9/§7）：异常台账 + 容忍度落点。
-- UNIQUE(event_fingerprint, merge_bucket) 仅作并发创建兜底——合并语义是应用层
-- UPDATE 先行、锚定 last_hit_at 的滚动 4h 窗（D16），固定桶不参与合并判定。
-- mysql 链独立编号（与 postgres 000097 / sqlite 000023 同形）。
CREATE TABLE behavior_anomalies (
    id                     BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_fingerprint      VARCHAR(64) NOT NULL,
    merge_bucket           BIGINT NOT NULL,
    entity_type            VARCHAR(16) NOT NULL,
    entity_id              BIGINT NOT NULL,
    org_id                 BIGINT NULL,
    score                  DOUBLE NOT NULL DEFAULT 0,
    score_peak             DOUBLE NOT NULL DEFAULT 0,
    band                   VARCHAR(16) NOT NULL DEFAULT 'suspicious',
    dimensions             TEXT NULL,
    baseline_model_version INT NOT NULL DEFAULT 1,
    first_seen_at          DATETIME(3) NOT NULL,
    last_hit_at            DATETIME(3) NOT NULL,
    hit_count              INT NOT NULL DEFAULT 1,
    window_start           DATETIME(3) NOT NULL,
    window_end             DATETIME(3) NOT NULL,
    prev_event_id          BIGINT NULL,
    status                 VARCHAR(16) NOT NULL DEFAULT 'open',
    closed_reason          VARCHAR(64) NULL,
    decided_by             VARCHAR(64) NULL,
    created_at             DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at             DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY idx_behavior_anomalies_dedup (event_fingerprint, merge_bucket),
    INDEX idx_behavior_anomalies_sweep (status, last_hit_at),
    INDEX idx_behavior_anomalies_entity (entity_type, entity_id, status)
);

-- 容忍度乘子（D10/D14）：confirmed/dismissed 反馈的唯一落点，永不改写基线数据。
CREATE TABLE behavior_tolerances (
    id               BIGINT AUTO_INCREMENT PRIMARY KEY,
    entity_type      VARCHAR(16) NOT NULL,
    entity_id        BIGINT NOT NULL,
    metric           VARCHAR(128) NOT NULL,
    multiplier       DOUBLE NOT NULL DEFAULT 1,
    last_feedback_at DATETIME(3) NOT NULL,
    created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY idx_behavior_tolerances_upsert (entity_type, entity_id, metric)
);
