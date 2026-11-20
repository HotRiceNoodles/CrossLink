-- 行为指纹候选库(M3 反馈闭环,冻结 v4.0 §5.6/D10)。postgres 链:清越社区(100)
-- 与 overlay(98)链尾。
CREATE TABLE behavior_fingerprints (
    id              BIGSERIAL PRIMARY KEY,
    entity_type     VARCHAR(16) NOT NULL,
    entity_id       BIGINT NOT NULL,
    dimension_set   VARCHAR(256) NOT NULL,
    template        TEXT NOT NULL,
    source_event_id BIGINT NULL,
    hit_count       INT NOT NULL DEFAULT 1,
    status          VARCHAR(16) NOT NULL DEFAULT 'candidate',
    decided_by      VARCHAR(64) NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idx_behavior_fps_dedupe UNIQUE (entity_type, entity_id, dimension_set)
);
CREATE INDEX idx_behavior_fps_status ON behavior_fingerprints (status);
