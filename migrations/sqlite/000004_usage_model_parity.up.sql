-- UsageLog model parity for SQLite (round-13 authorization). The squashed
-- init (000001, historical, byte-identical) predates 21 model columns and
-- types created_at as TEXT, which the sqlite driver cannot scan back into
-- the model's time.Time (confirmed drift class). SQLite cannot ALTER a
-- column type, so this migration performs the official table-rebuild
-- procedure: new table carries every current column (parity columns from the
-- PostgreSQL chain 000070-000077 + guardrail_truncated from 000003) with
-- created_at as DATETIME; existing rows are copied; indexes recreated.
CREATE TABLE usage_logs_new (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT NOT NULL,
    api_key_id        INTEGER REFERENCES api_keys(id) ON DELETE SET NULL,
    provider_id       INTEGER REFERENCES providers(id) ON DELETE SET NULL,
    route_type        TEXT NOT NULL,
    model_requested   TEXT NOT NULL,
    model_used        TEXT NOT NULL,
    input_tokens      INTEGER NOT NULL DEFAULT 0,
    output_tokens     INTEGER NOT NULL DEFAULT 0,
    cost              REAL NOT NULL DEFAULT 0,
    billable_cost     REAL NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    first_token_ms    INTEGER,
    status_code       INTEGER NOT NULL,
    error_type        TEXT,
    error_message     TEXT,
    upstream_status   INTEGER,
    upstream_error_code TEXT,
    upstream_error_type TEXT,
    upstream_error_param TEXT,
    error_stage       TEXT,
    attempts          TEXT,
    currency          TEXT NOT NULL DEFAULT 'CNY',
    team_id           INTEGER REFERENCES teams(id) ON DELETE SET NULL,
    user_message      TEXT,
    model_response    TEXT,
    fallback_count    INTEGER NOT NULL DEFAULT 0,
    retry_count       INTEGER NOT NULL DEFAULT 0,
    guardrail_triggered INTEGER DEFAULT 0,
    guardrail_rule    TEXT,
    guardrail_truncated INTEGER NOT NULL DEFAULT 0,
    cache_hit         INTEGER NOT NULL DEFAULT 0,
    agent_type        TEXT,
    security_events   TEXT DEFAULT '[]',
    org_id            INTEGER REFERENCES organizations(id),
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    session_id        TEXT,
    template_id       INTEGER,
    image_count       INTEGER,
    image_size        TEXT,
    image_quality     TEXT,
    system_tokens     INTEGER,
    history_tokens    INTEGER,
    question_tokens   INTEGER,
    tool_tokens       INTEGER,
    tool_output_tokens INTEGER,
    context_window    INTEGER,
    context_utilization_bp INTEGER,
    analysis_flags    INTEGER,
    context_snapshot  TEXT,
    created_at        DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO usage_logs_new (id, request_id, api_key_id, provider_id, route_type, model_requested,
    model_used, input_tokens, output_tokens, cost, latency_ms, first_token_ms, status_code,
    error_type, currency, team_id, user_message, model_response, fallback_count, retry_count,
    guardrail_triggered, guardrail_rule, guardrail_truncated, cache_hit, agent_type, security_events,
    org_id, reasoning_tokens, cache_read_tokens, session_id, created_at)
SELECT id, request_id, api_key_id, provider_id, route_type, model_requested,
    model_used, input_tokens, output_tokens, cost, latency_ms, first_token_ms, status_code,
    error_type, currency, team_id, user_message, model_response, fallback_count, retry_count,
    guardrail_triggered, guardrail_rule, guardrail_truncated, cache_hit, agent_type, security_events,
    org_id, reasoning_tokens, cache_read_tokens, session_id, created_at FROM usage_logs;
DROP TABLE usage_logs;
ALTER TABLE usage_logs_new RENAME TO usage_logs;
CREATE INDEX IF NOT EXISTS idx_usage_logs_api_key_id ON usage_logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_provider_id ON usage_logs(provider_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_usage_logs_model_requested ON usage_logs(model_requested);
CREATE INDEX IF NOT EXISTS idx_usage_logs_currency ON usage_logs(currency);
CREATE INDEX IF NOT EXISTS idx_usage_logs_team_id ON usage_logs(team_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_fallback ON usage_logs(fallback_count) WHERE fallback_count > 0;
CREATE INDEX IF NOT EXISTS idx_usage_logs_retry ON usage_logs(retry_count) WHERE retry_count > 0;
CREATE INDEX IF NOT EXISTS idx_usage_logs_guardrail ON usage_logs(guardrail_triggered) WHERE guardrail_triggered = 1;
CREATE INDEX IF NOT EXISTS idx_usage_logs_cache_hit ON usage_logs(cache_hit) WHERE cache_hit = 1;
CREATE INDEX IF NOT EXISTS idx_usage_logs_session_id ON usage_logs(session_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_agent_type ON usage_logs(agent_type) WHERE agent_type IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_usage_logs_org_id ON usage_logs(org_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_status_code ON usage_logs(status_code);
CREATE INDEX IF NOT EXISTS idx_usage_logs_error_type ON usage_logs(error_type) WHERE error_type IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_usage_logs_template_id ON usage_logs(template_id);
