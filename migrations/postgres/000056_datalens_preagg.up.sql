-- DataLens: Pre-aggregation tables for BI analytics

-- ═══ Hourly pre-aggregation ═══
CREATE TABLE datalens_hourly_metrics (
    id              BIGSERIAL PRIMARY KEY,
    org_id          BIGINT NOT NULL,
    agg_level       VARCHAR(16) NOT NULL,
    team_id         BIGINT,
    api_key_id      BIGINT,
    provider_id     BIGINT,
    model_name      VARCHAR(128),
    route_type      VARCHAR(16),
    status_group    SMALLINT NOT NULL DEFAULT 200,
    hour_bucket     TIMESTAMPTZ NOT NULL,
    currency        VARCHAR(3) NOT NULL DEFAULT 'CNY',

    request_count       INT     NOT NULL DEFAULT 0,
    input_tokens        BIGINT  NOT NULL DEFAULT 0,
    output_tokens       BIGINT  NOT NULL DEFAULT 0,
    reasoning_tokens    BIGINT  NOT NULL DEFAULT 0,
    cache_read_tokens   BIGINT  NOT NULL DEFAULT 0,
    total_cost          DECIMAL(20,8) NOT NULL DEFAULT 0,
    total_latency_ms    BIGINT  NOT NULL DEFAULT 0,
    min_latency_ms      INT     NOT NULL DEFAULT 0,
    max_latency_ms      INT     NOT NULL DEFAULT 0,
    latency_samples     INT     NOT NULL DEFAULT 0,
    first_token_samples INT     NOT NULL DEFAULT 0,
    total_first_token_ms BIGINT NOT NULL DEFAULT 0,
    error_count         INT     NOT NULL DEFAULT 0,
    fallback_count      INT     NOT NULL DEFAULT 0,
    retry_count         INT     NOT NULL DEFAULT 0,
    guardrail_blocks    INT     NOT NULL DEFAULT 0,
    cache_hits          INT     NOT NULL DEFAULT 0,
    distinct_sessions   INT     NOT NULL DEFAULT 0,
    distinct_keys       INT     NOT NULL DEFAULT 0,

    latency_bucket_50   INT NOT NULL DEFAULT 0,
    latency_bucket_100  INT NOT NULL DEFAULT 0,
    latency_bucket_200  INT NOT NULL DEFAULT 0,
    latency_bucket_500  INT NOT NULL DEFAULT 0,
    latency_bucket_1000 INT NOT NULL DEFAULT 0,
    latency_bucket_2000 INT NOT NULL DEFAULT 0,
    latency_bucket_5000 INT NOT NULL DEFAULT 0,
    latency_bucket_slow INT NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_dhm_upsert ON datalens_hourly_metrics (
    org_id, agg_level,
    COALESCE(team_id, -1), COALESCE(api_key_id, -1),
    COALESCE(provider_id, -1), COALESCE(model_name, ''),
    COALESCE(route_type, ''), status_group, hour_bucket, currency
);

CREATE INDEX idx_dhm_org_hour      ON datalens_hourly_metrics (org_id, hour_bucket);
CREATE INDEX idx_dhm_level_hour    ON datalens_hourly_metrics (org_id, agg_level, hour_bucket);
CREATE INDEX idx_dhm_model_hour    ON datalens_hourly_metrics (org_id, model_name, hour_bucket)
    WHERE model_name IS NOT NULL;
CREATE INDEX idx_dhm_team_hour     ON datalens_hourly_metrics (org_id, team_id, hour_bucket)
    WHERE team_id IS NOT NULL;
CREATE INDEX idx_dhm_key_hour      ON datalens_hourly_metrics (org_id, api_key_id, hour_bucket)
    WHERE api_key_id IS NOT NULL;
CREATE INDEX idx_dhm_provider_hour ON datalens_hourly_metrics (org_id, provider_id, hour_bucket)
    WHERE provider_id IS NOT NULL;

-- ═══ Daily pre-aggregation ═══
CREATE TABLE datalens_daily_metrics (
    id              BIGSERIAL PRIMARY KEY,
    org_id          BIGINT NOT NULL,
    agg_level       VARCHAR(16) NOT NULL,
    team_id         BIGINT,
    api_key_id      BIGINT,
    provider_id     BIGINT,
    model_name      VARCHAR(128),
    route_type      VARCHAR(16),
    status_group    SMALLINT NOT NULL DEFAULT 200,
    day_bucket      DATE NOT NULL,
    currency        VARCHAR(3) NOT NULL DEFAULT 'CNY',

    request_count       INT     NOT NULL DEFAULT 0,
    input_tokens        BIGINT  NOT NULL DEFAULT 0,
    output_tokens       BIGINT  NOT NULL DEFAULT 0,
    reasoning_tokens    BIGINT  NOT NULL DEFAULT 0,
    cache_read_tokens   BIGINT  NOT NULL DEFAULT 0,
    total_cost          DECIMAL(20,8) NOT NULL DEFAULT 0,
    total_latency_ms    BIGINT  NOT NULL DEFAULT 0,
    min_latency_ms      INT     NOT NULL DEFAULT 0,
    max_latency_ms      INT     NOT NULL DEFAULT 0,
    latency_samples     INT     NOT NULL DEFAULT 0,
    first_token_samples INT     NOT NULL DEFAULT 0,
    total_first_token_ms BIGINT NOT NULL DEFAULT 0,
    error_count         INT     NOT NULL DEFAULT 0,
    fallback_count      INT     NOT NULL DEFAULT 0,
    retry_count         INT     NOT NULL DEFAULT 0,
    guardrail_blocks    INT     NOT NULL DEFAULT 0,
    cache_hits          INT     NOT NULL DEFAULT 0,
    distinct_sessions   INT     NOT NULL DEFAULT 0,
    distinct_keys       INT     NOT NULL DEFAULT 0,

    latency_bucket_50   INT NOT NULL DEFAULT 0,
    latency_bucket_100  INT NOT NULL DEFAULT 0,
    latency_bucket_200  INT NOT NULL DEFAULT 0,
    latency_bucket_500  INT NOT NULL DEFAULT 0,
    latency_bucket_1000 INT NOT NULL DEFAULT 0,
    latency_bucket_2000 INT NOT NULL DEFAULT 0,
    latency_bucket_5000 INT NOT NULL DEFAULT 0,
    latency_bucket_slow INT NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_ddm_upsert ON datalens_daily_metrics (
    org_id, agg_level,
    COALESCE(team_id, -1), COALESCE(api_key_id, -1),
    COALESCE(provider_id, -1), COALESCE(model_name, ''),
    COALESCE(route_type, ''), status_group, day_bucket, currency
);

CREATE INDEX idx_ddm_org_day ON datalens_daily_metrics (org_id, day_bucket);

-- ═══ Aggregation health status ═══
CREATE TABLE datalens_agg_status (
    id              BIGSERIAL PRIMARY KEY,
    agg_level       VARCHAR(16) NOT NULL,
    agg_type        VARCHAR(8) NOT NULL,
    last_success_at TIMESTAMPTZ NOT NULL,
    last_duration_ms INT NOT NULL DEFAULT 0,
    rows_affected   INT NOT NULL DEFAULT 0,
    error_message   TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_das_level_type ON datalens_agg_status (agg_level, agg_type);

-- ═══ Saved report configurations ═══
CREATE TABLE datalens_reports (
    id          BIGSERIAL PRIMARY KEY,
    org_id      BIGINT NOT NULL,
    user_id     BIGINT NOT NULL,
    name        VARCHAR(128) NOT NULL,
    description TEXT,
    type        VARCHAR(16) NOT NULL DEFAULT 'custom',
    template_id VARCHAR(64),
    scope       VARCHAR(16) NOT NULL DEFAULT 'private',
    config      JSONB NOT NULL,
    is_pinned   BOOLEAN NOT NULL DEFAULT false,
    version     INT NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX idx_dr_org_user ON datalens_reports (org_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_dr_template ON datalens_reports (org_id, template_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_dr_deleted  ON datalens_reports (deleted_at) WHERE deleted_at IS NOT NULL;

-- ═══ Automated report schedules (Enterprise) ═══
CREATE TABLE datalens_schedules (
    id          BIGSERIAL PRIMARY KEY,
    org_id      BIGINT NOT NULL,
    user_id     BIGINT NOT NULL,
    team_id     BIGINT,
    name        VARCHAR(128) NOT NULL,
    report_id   BIGINT NOT NULL,
    cron        VARCHAR(64) NOT NULL,
    timezone    VARCHAR(32) NOT NULL DEFAULT 'Asia/Shanghai',
    channels    JSONB NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    last_run_at TIMESTAMPTZ,
    next_run_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX idx_ds_org ON datalens_schedules (org_id, enabled, next_run_at) WHERE deleted_at IS NULL;
