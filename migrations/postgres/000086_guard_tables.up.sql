-- 旁路检测服务（crosslink-guard，设计 DD-5/DD-6）：接入凭证、事件留存、trace 元数据。
-- guard_events 只存脱敏摘要（SanitizePreview）与 SHA-256 哈希，不存原文。
CREATE TABLE IF NOT EXISTS guard_ingest_tokens (
    id BIGSERIAL PRIMARY KEY,
    org_id BIGINT NOT NULL,
    name VARCHAR(128) NOT NULL DEFAULT '',
    token_hash VARCHAR(64) NOT NULL,
    key_prefix VARCHAR(16) NOT NULL DEFAULT '',
    hash_algo VARCHAR(10) NOT NULL DEFAULT 'sha256',
    status SMALLINT NOT NULL DEFAULT 1,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_guard_ingest_tokens_hash ON guard_ingest_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_guard_ingest_tokens_org ON guard_ingest_tokens(org_id);

CREATE TABLE IF NOT EXISTS guard_events (
    id BIGSERIAL PRIMARY KEY,
    trace_id VARCHAR(64) NOT NULL DEFAULT '',
    org_id BIGINT NOT NULL DEFAULT 0,
    rule_id BIGINT NOT NULL DEFAULT 0,
    rule_version BIGINT NOT NULL DEFAULT 0,
    engine_type VARCHAR(50) NOT NULL DEFAULT '',
    direction VARCHAR(10) NOT NULL,
    action VARCHAR(20) NOT NULL,
    severity VARCHAR(20) NOT NULL DEFAULT '',
    reason VARCHAR(1000) NOT NULL DEFAULT '',
    content_preview VARCHAR(500) NOT NULL DEFAULT '',
    content_sha256 VARCHAR(64) NOT NULL DEFAULT '',
    model VARCHAR(255) NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_guard_events_org ON guard_events(org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_guard_events_trace ON guard_events(trace_id);

CREATE TABLE IF NOT EXISTS guard_traces (
    id BIGSERIAL PRIMARY KEY,
    trace_id VARCHAR(64) NOT NULL,
    org_id BIGINT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'open',
    verdict VARCHAR(20) NOT NULL DEFAULT 'pending',
    request_at TIMESTAMPTZ,
    response_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_guard_traces_org_trace ON guard_traces(org_id, trace_id);
CREATE INDEX IF NOT EXISTS idx_guard_traces_org_created ON guard_traces(org_id, created_at DESC);
