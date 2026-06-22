CREATE TABLE IF NOT EXISTS capabilities (
    id          BIGSERIAL PRIMARY KEY,
    org_id      BIGINT      NOT NULL DEFAULT 0,
    name        VARCHAR(64) NOT NULL,
    modality    VARCHAR(16) NOT NULL DEFAULT 'text',
    status      SMALLINT    NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT capabilities_org_name_unique UNIQUE (org_id, name)
);

CREATE TABLE IF NOT EXISTS capability_members (
    id             BIGSERIAL PRIMARY KEY,
    capability_id  BIGINT      NOT NULL REFERENCES capabilities(id) ON DELETE CASCADE,
    model_name     VARCHAR(128) NOT NULL,
    quality_score  INTEGER     NOT NULL DEFAULT 0,
    status         SMALLINT    NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT capability_members_cap_model_unique UNIQUE (capability_id, model_name)
);
CREATE INDEX idx_capability_members_cap ON capability_members (capability_id);
CREATE INDEX idx_capability_members_quality ON capability_members (capability_id, quality_score DESC);
