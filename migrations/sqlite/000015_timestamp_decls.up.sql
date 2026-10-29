-- SQLite only: rebuild every table whose timestamp columns are declared TEXT
-- into fresh tables with DATETIME declarations. The glebarez driver maps
-- columns by DECLARED type — TEXT comes back as string, DATETIME as time.Time —
-- so a TEXT created_at breaks GORM full-row scans of time.Time models after a
-- restart (scan error: unsupported type string → time.Time). Values are copied
-- through datetime() so surviving rows land in a single canonical format.
-- Follows the 000012_guardrail_rules_datetime rebuild pattern, generalized.
--
-- This file must run with FK enforcement suspended: DROP TABLE on a referenced
-- parent table does an implicit DELETE that trips the FK violation counter even
-- though the replacement table takes the name and rows back before COMMIT, so
-- the commit fails. The runner detects the pragma below (a no-op inside the
-- wrapping transaction, kept as documentation) and executes it — and the
-- matching re-enable at the end — outside the transaction.
PRAGMA foreign_keys = OFF;

-- ==== agent_fingerprints ====
CREATE TABLE agent_fingerprints_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    source_type     TEXT NOT NULL DEFAULT 'header',
    source_field    TEXT NOT NULL DEFAULT '',
    pattern         TEXT NOT NULL,
    risk_level      TEXT NOT NULL DEFAULT 'medium',
    origin          TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    hit_count       INTEGER NOT NULL DEFAULT 0,
    last_hit_at DATETIME,
    discovered_from TEXT,
    org_id          INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO agent_fingerprints_rebuild ("id", "name", "source_type", "source_field", "pattern", "risk_level", "origin", "status", "hit_count", "last_hit_at", "discovered_from", "org_id", "created_at", "updated_at")
SELECT "id", "name", "source_type", "source_field", "pattern", "risk_level", "origin", "status", "hit_count", datetime("last_hit_at"), "discovered_from", "org_id", datetime("created_at"), datetime("updated_at")
FROM agent_fingerprints;
DROP TABLE agent_fingerprints;
ALTER TABLE agent_fingerprints_rebuild RENAME TO agent_fingerprints;
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_fingerprints_dedup
    ON agent_fingerprints(source_type, source_field, pattern);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_name ON agent_fingerprints(name);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_origin ON agent_fingerprints(origin);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_status ON agent_fingerprints(status);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_status_type ON agent_fingerprints(status, source_type, source_field);

-- ==== api_key_hashes ====
CREATE TABLE api_key_hashes_rebuild (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id  INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    key_hash    TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    hash_algo   TEXT NOT NULL DEFAULT 'sha256',
    is_primary  INTEGER NOT NULL DEFAULT 1,
    grace_until DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO api_key_hashes_rebuild ("id", "api_key_id", "key_hash", "key_prefix", "hash_algo", "is_primary", "grace_until", "created_at")
SELECT "id", "api_key_id", "key_hash", "key_prefix", "hash_algo", "is_primary", datetime("grace_until"), datetime("created_at")
FROM api_key_hashes;
DROP TABLE api_key_hashes;
ALTER TABLE api_key_hashes_rebuild RENAME TO api_key_hashes;
CREATE INDEX IF NOT EXISTS idx_api_key_hashes_api_key_id ON api_key_hashes(api_key_id);
CREATE INDEX IF NOT EXISTS idx_api_key_hashes_grace_until ON api_key_hashes(grace_until) WHERE grace_until IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_key_hashes_key_hash ON api_key_hashes(key_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_key_hashes_one_primary ON api_key_hashes(api_key_id) WHERE is_primary = 1;

-- ==== api_keys ====
CREATE TABLE api_keys_rebuild (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL,
    key_hash            TEXT UNIQUE NOT NULL,
    key_prefix          TEXT NOT NULL,
    status              INTEGER NOT NULL DEFAULT 1,
    allowed_models      TEXT,
    allowed_routes      TEXT,
    tpm_limit           INTEGER NOT NULL DEFAULT 0,
    rpm_limit           INTEGER NOT NULL DEFAULT 0,
    max_budget          REAL NOT NULL DEFAULT 0,
    budget_period       TEXT NOT NULL DEFAULT 'monthly',
    max_calls           INTEGER NOT NULL DEFAULT 0,
    call_period         TEXT NOT NULL DEFAULT 'daily',
    email               TEXT,
    created_by          TEXT NOT NULL DEFAULT 'admin',
    created_by_id       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    team_id             INTEGER REFERENCES teams(id) ON DELETE SET NULL,
    org_id              INTEGER REFERENCES organizations(id),
    expires_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    last_used_at DATETIME,
    deleted_at DATETIME
, allowed_ips TEXT, key_source TEXT NOT NULL DEFAULT 'native', source_vendor TEXT NOT NULL DEFAULT '', price_multiplier DECIMAL(6,4) NOT NULL DEFAULT 1.0);
INSERT INTO api_keys_rebuild ("id", "name", "key_hash", "key_prefix", "status", "allowed_models", "allowed_routes", "tpm_limit", "rpm_limit", "max_budget", "budget_period", "max_calls", "call_period", "email", "created_by", "created_by_id", "team_id", "org_id", "expires_at", "created_at", "updated_at", "last_used_at", "deleted_at", "allowed_ips", "key_source", "source_vendor", "price_multiplier")
SELECT "id", "name", "key_hash", "key_prefix", "status", "allowed_models", "allowed_routes", "tpm_limit", "rpm_limit", "max_budget", "budget_period", "max_calls", "call_period", "email", "created_by", "created_by_id", "team_id", "org_id", datetime("expires_at"), datetime("created_at"), datetime("updated_at"), datetime("last_used_at"), datetime("deleted_at"), "allowed_ips", "key_source", "source_vendor", "price_multiplier"
FROM api_keys;
DROP TABLE api_keys;
ALTER TABLE api_keys_rebuild RENAME TO api_keys;
CREATE INDEX IF NOT EXISTS idx_api_keys_created_by_id ON api_keys(created_by_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_deleted_at ON api_keys(deleted_at);
CREATE INDEX IF NOT EXISTS idx_api_keys_org_id ON api_keys(org_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_team_id ON api_keys(team_id);

-- ==== audit_logs ====
CREATE TABLE audit_logs_rebuild (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id        INTEGER NOT NULL,
    username       TEXT NOT NULL,
    action         TEXT NOT NULL,
    resource_type  TEXT NOT NULL,
    resource_id    TEXT NOT NULL,
    resource_name  TEXT,
    detail         TEXT,
    ip_address     TEXT,
    user_agent     TEXT,
    status         TEXT NOT NULL DEFAULT 'success',
    org_id         INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO audit_logs_rebuild ("id", "user_id", "username", "action", "resource_type", "resource_id", "resource_name", "detail", "ip_address", "user_agent", "status", "org_id", "created_at")
SELECT "id", "user_id", "username", "action", "resource_type", "resource_id", "resource_name", "detail", "ip_address", "user_agent", "status", "org_id", datetime("created_at")
FROM audit_logs;
DROP TABLE audit_logs;
ALTER TABLE audit_logs_rebuild RENAME TO audit_logs;
CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs(action);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action_created ON audit_logs(action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_logs_org_id ON audit_logs(org_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_resource ON audit_logs(resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_resource_type_created ON audit_logs(resource_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_user_id ON audit_logs(user_id);

-- ==== budget_alerts ====
CREATE TABLE budget_alerts_rebuild (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    team_id          INTEGER REFERENCES teams(id) ON DELETE CASCADE,
    key_id           INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    org_id           INTEGER,
    threshold_pct    INTEGER NOT NULL,
    webhook_url      TEXT NOT NULL,
    last_triggered_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO budget_alerts_rebuild ("id", "team_id", "key_id", "org_id", "threshold_pct", "webhook_url", "last_triggered_at", "created_at", "deleted_at")
SELECT "id", "team_id", "key_id", "org_id", "threshold_pct", "webhook_url", datetime("last_triggered_at"), datetime("created_at"), datetime("deleted_at")
FROM budget_alerts;
DROP TABLE budget_alerts;
ALTER TABLE budget_alerts_rebuild RENAME TO budget_alerts;
CREATE INDEX IF NOT EXISTS idx_budget_alerts_deleted_at ON budget_alerts(deleted_at);
CREATE INDEX IF NOT EXISTS idx_budget_alerts_key ON budget_alerts(key_id);
CREATE INDEX IF NOT EXISTS idx_budget_alerts_team ON budget_alerts(team_id);

-- ==== budget_recommendations ====
CREATE TABLE budget_recommendations_rebuild (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type       TEXT NOT NULL,
    target_id         INTEGER NOT NULL,
    period            TEXT NOT NULL DEFAULT 'monthly',
    recommended_budget REAL NOT NULL,
    current_budget    REAL NOT NULL DEFAULT 0,
    avg_period_spend  REAL NOT NULL DEFAULT 0,
    growth_rate       REAL NOT NULL DEFAULT 0,
    confidence        REAL NOT NULL DEFAULT 0,
    reasoning         TEXT NOT NULL DEFAULT '',
    currency          TEXT NOT NULL DEFAULT 'CNY',
    org_id            INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO budget_recommendations_rebuild ("id", "target_type", "target_id", "period", "recommended_budget", "current_budget", "avg_period_spend", "growth_rate", "confidence", "reasoning", "currency", "org_id", "created_at")
SELECT "id", "target_type", "target_id", "period", "recommended_budget", "current_budget", "avg_period_spend", "growth_rate", "confidence", "reasoning", "currency", "org_id", datetime("created_at")
FROM budget_recommendations;
DROP TABLE budget_recommendations;
ALTER TABLE budget_recommendations_rebuild RENAME TO budget_recommendations;
CREATE INDEX IF NOT EXISTS idx_budget_recommendations_org_id ON budget_recommendations(org_id);
CREATE INDEX IF NOT EXISTS idx_budget_recs_created ON budget_recommendations(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_budget_recs_target ON budget_recommendations(target_type, target_id);

-- ==== budget_requests ====
CREATE TABLE budget_requests_rebuild (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type        TEXT NOT NULL,
    target_id          INTEGER NOT NULL,
    period             TEXT NOT NULL DEFAULT 'monthly',
    current_budget     REAL NOT NULL DEFAULT 0,
    requested_budget   REAL NOT NULL,
    reason             TEXT NOT NULL DEFAULT '',
    recommendation_id  INTEGER,
    status             TEXT NOT NULL DEFAULT 'pending',
    created_by         INTEGER NOT NULL,
    reviewed_by        INTEGER,
    review_comment     TEXT NOT NULL DEFAULT '',
    reviewed_at DATETIME,
    org_id             INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO budget_requests_rebuild ("id", "target_type", "target_id", "period", "current_budget", "requested_budget", "reason", "recommendation_id", "status", "created_by", "reviewed_by", "review_comment", "reviewed_at", "org_id", "created_at", "updated_at")
SELECT "id", "target_type", "target_id", "period", "current_budget", "requested_budget", "reason", "recommendation_id", "status", "created_by", "reviewed_by", "review_comment", datetime("reviewed_at"), "org_id", datetime("created_at"), datetime("updated_at")
FROM budget_requests;
DROP TABLE budget_requests;
ALTER TABLE budget_requests_rebuild RENAME TO budget_requests;
CREATE INDEX IF NOT EXISTS idx_budget_reqs_created ON budget_requests(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_creator ON budget_requests(created_by);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_reviewer ON budget_requests(reviewed_by);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_status ON budget_requests(status);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_target ON budget_requests(target_type, target_id);
CREATE INDEX IF NOT EXISTS idx_budget_requests_org_id ON budget_requests(org_id);

-- ==== budget_snapshots ====
CREATE TABLE budget_snapshots_rebuild (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type TEXT NOT NULL,
    target_id   INTEGER NOT NULL,
    period_key  TEXT NOT NULL,
    spent       REAL NOT NULL DEFAULT 0,
    budget      REAL NOT NULL DEFAULT 0,
    currency    TEXT NOT NULL DEFAULT 'CNY',
    org_id      INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    UNIQUE (target_type, target_id, period_key)
);
INSERT INTO budget_snapshots_rebuild ("id", "target_type", "target_id", "period_key", "spent", "budget", "currency", "org_id", "created_at")
SELECT "id", "target_type", "target_id", "period_key", "spent", "budget", "currency", "org_id", datetime("created_at")
FROM budget_snapshots;
DROP TABLE budget_snapshots;
ALTER TABLE budget_snapshots_rebuild RENAME TO budget_snapshots;
CREATE INDEX IF NOT EXISTS idx_budget_snapshots_org_id ON budget_snapshots(org_id);

-- ==== datalens_agg_status ====
CREATE TABLE datalens_agg_status_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agg_level       TEXT NOT NULL,
    agg_type        TEXT NOT NULL,
    last_success_at DATETIME NOT NULL,
    last_duration_ms INTEGER NOT NULL DEFAULT 0,
    rows_affected   INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT,
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO datalens_agg_status_rebuild ("id", "agg_level", "agg_type", "last_success_at", "last_duration_ms", "rows_affected", "error_message", "updated_at")
SELECT "id", "agg_level", "agg_type", datetime("last_success_at"), "last_duration_ms", "rows_affected", "error_message", datetime("updated_at")
FROM datalens_agg_status;
DROP TABLE datalens_agg_status;
ALTER TABLE datalens_agg_status_rebuild RENAME TO datalens_agg_status;
CREATE UNIQUE INDEX IF NOT EXISTS idx_das_level_type ON datalens_agg_status (agg_level, agg_type);

-- ==== datalens_daily_metrics ====
CREATE TABLE datalens_daily_metrics_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL,
    agg_level       TEXT NOT NULL,
    team_id         INTEGER,
    api_key_id      INTEGER,
    provider_id     INTEGER,
    model_name      TEXT,
    route_type      TEXT,
    status_group    INTEGER NOT NULL DEFAULT 200,
    day_bucket DATETIME NOT NULL,
    currency        TEXT NOT NULL DEFAULT 'CNY',
    request_count       INTEGER NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    total_cost          REAL    NOT NULL DEFAULT 0,
    total_latency_ms    INTEGER NOT NULL DEFAULT 0,
    min_latency_ms      INTEGER NOT NULL DEFAULT 0,
    max_latency_ms      INTEGER NOT NULL DEFAULT 0,
    latency_samples     INTEGER NOT NULL DEFAULT 0,
    first_token_samples INTEGER NOT NULL DEFAULT 0,
    total_first_token_ms INTEGER NOT NULL DEFAULT 0,
    error_count         INTEGER NOT NULL DEFAULT 0,
    fallback_count      INTEGER NOT NULL DEFAULT 0,
    retry_count         INTEGER NOT NULL DEFAULT 0,
    guardrail_blocks    INTEGER NOT NULL DEFAULT 0,
    cache_hits          INTEGER NOT NULL DEFAULT 0,
    distinct_sessions   INTEGER NOT NULL DEFAULT 0,
    distinct_keys       INTEGER NOT NULL DEFAULT 0,
    latency_bucket_50   INTEGER NOT NULL DEFAULT 0,
    latency_bucket_100  INTEGER NOT NULL DEFAULT 0,
    latency_bucket_200  INTEGER NOT NULL DEFAULT 0,
    latency_bucket_500  INTEGER NOT NULL DEFAULT 0,
    latency_bucket_1000 INTEGER NOT NULL DEFAULT 0,
    latency_bucket_2000 INTEGER NOT NULL DEFAULT 0,
    latency_bucket_5000 INTEGER NOT NULL DEFAULT 0,
    latency_bucket_slow INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
, ctx_system_tokens INTEGER NOT NULL DEFAULT 0, ctx_history_tokens INTEGER NOT NULL DEFAULT 0, ctx_question_tokens INTEGER NOT NULL DEFAULT 0, ctx_tool_tokens INTEGER NOT NULL DEFAULT 0, ctx_tool_output_tokens INTEGER NOT NULL DEFAULT 0, ctx_total_window INTEGER NOT NULL DEFAULT 0, ctx_analyzed_count INTEGER NOT NULL DEFAULT 0, ctx_overflow_count INTEGER NOT NULL DEFAULT 0, ctx_window_unknown_count INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_lt50 INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_50_80 INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_80_95 INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_gt95 INTEGER NOT NULL DEFAULT 0);
INSERT INTO datalens_daily_metrics_rebuild ("id", "org_id", "agg_level", "team_id", "api_key_id", "provider_id", "model_name", "route_type", "status_group", "day_bucket", "currency", "request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples", "first_token_samples", "total_first_token_ms", "error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits", "distinct_sessions", "distinct_keys", "latency_bucket_50", "latency_bucket_100", "latency_bucket_200", "latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000", "latency_bucket_5000", "latency_bucket_slow", "created_at", "ctx_system_tokens", "ctx_history_tokens", "ctx_question_tokens", "ctx_tool_tokens", "ctx_tool_output_tokens", "ctx_total_window", "ctx_analyzed_count", "ctx_overflow_count", "ctx_window_unknown_count", "ctx_util_bucket_lt50", "ctx_util_bucket_50_80", "ctx_util_bucket_80_95", "ctx_util_bucket_gt95")
SELECT "id", "org_id", "agg_level", "team_id", "api_key_id", "provider_id", "model_name", "route_type", "status_group", datetime("day_bucket"), "currency", "request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples", "first_token_samples", "total_first_token_ms", "error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits", "distinct_sessions", "distinct_keys", "latency_bucket_50", "latency_bucket_100", "latency_bucket_200", "latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000", "latency_bucket_5000", "latency_bucket_slow", datetime("created_at"), "ctx_system_tokens", "ctx_history_tokens", "ctx_question_tokens", "ctx_tool_tokens", "ctx_tool_output_tokens", "ctx_total_window", "ctx_analyzed_count", "ctx_overflow_count", "ctx_window_unknown_count", "ctx_util_bucket_lt50", "ctx_util_bucket_50_80", "ctx_util_bucket_80_95", "ctx_util_bucket_gt95"
FROM datalens_daily_metrics;
DROP TABLE datalens_daily_metrics;
ALTER TABLE datalens_daily_metrics_rebuild RENAME TO datalens_daily_metrics;
CREATE INDEX IF NOT EXISTS idx_ddm_org_day ON datalens_daily_metrics (org_id, day_bucket);

-- ==== datalens_hourly_metrics ====
CREATE TABLE datalens_hourly_metrics_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL,
    agg_level       TEXT NOT NULL,
    team_id         INTEGER,
    api_key_id      INTEGER,
    provider_id     INTEGER,
    model_name      TEXT,
    route_type      TEXT,
    status_group    INTEGER NOT NULL DEFAULT 200,
    hour_bucket DATETIME NOT NULL,
    currency        TEXT NOT NULL DEFAULT 'CNY',
    request_count       INTEGER NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    total_cost          REAL    NOT NULL DEFAULT 0,
    total_latency_ms    INTEGER NOT NULL DEFAULT 0,
    min_latency_ms      INTEGER NOT NULL DEFAULT 0,
    max_latency_ms      INTEGER NOT NULL DEFAULT 0,
    latency_samples     INTEGER NOT NULL DEFAULT 0,
    first_token_samples INTEGER NOT NULL DEFAULT 0,
    total_first_token_ms INTEGER NOT NULL DEFAULT 0,
    error_count         INTEGER NOT NULL DEFAULT 0,
    fallback_count      INTEGER NOT NULL DEFAULT 0,
    retry_count         INTEGER NOT NULL DEFAULT 0,
    guardrail_blocks    INTEGER NOT NULL DEFAULT 0,
    cache_hits          INTEGER NOT NULL DEFAULT 0,
    distinct_sessions   INTEGER NOT NULL DEFAULT 0,
    distinct_keys       INTEGER NOT NULL DEFAULT 0,
    latency_bucket_50   INTEGER NOT NULL DEFAULT 0,
    latency_bucket_100  INTEGER NOT NULL DEFAULT 0,
    latency_bucket_200  INTEGER NOT NULL DEFAULT 0,
    latency_bucket_500  INTEGER NOT NULL DEFAULT 0,
    latency_bucket_1000 INTEGER NOT NULL DEFAULT 0,
    latency_bucket_2000 INTEGER NOT NULL DEFAULT 0,
    latency_bucket_5000 INTEGER NOT NULL DEFAULT 0,
    latency_bucket_slow INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
, ctx_system_tokens INTEGER NOT NULL DEFAULT 0, ctx_history_tokens INTEGER NOT NULL DEFAULT 0, ctx_question_tokens INTEGER NOT NULL DEFAULT 0, ctx_tool_tokens INTEGER NOT NULL DEFAULT 0, ctx_tool_output_tokens INTEGER NOT NULL DEFAULT 0, ctx_total_window INTEGER NOT NULL DEFAULT 0, ctx_analyzed_count INTEGER NOT NULL DEFAULT 0, ctx_overflow_count INTEGER NOT NULL DEFAULT 0, ctx_window_unknown_count INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_lt50 INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_50_80 INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_80_95 INTEGER NOT NULL DEFAULT 0, ctx_util_bucket_gt95 INTEGER NOT NULL DEFAULT 0);
INSERT INTO datalens_hourly_metrics_rebuild ("id", "org_id", "agg_level", "team_id", "api_key_id", "provider_id", "model_name", "route_type", "status_group", "hour_bucket", "currency", "request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples", "first_token_samples", "total_first_token_ms", "error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits", "distinct_sessions", "distinct_keys", "latency_bucket_50", "latency_bucket_100", "latency_bucket_200", "latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000", "latency_bucket_5000", "latency_bucket_slow", "created_at", "ctx_system_tokens", "ctx_history_tokens", "ctx_question_tokens", "ctx_tool_tokens", "ctx_tool_output_tokens", "ctx_total_window", "ctx_analyzed_count", "ctx_overflow_count", "ctx_window_unknown_count", "ctx_util_bucket_lt50", "ctx_util_bucket_50_80", "ctx_util_bucket_80_95", "ctx_util_bucket_gt95")
SELECT "id", "org_id", "agg_level", "team_id", "api_key_id", "provider_id", "model_name", "route_type", "status_group", datetime("hour_bucket"), "currency", "request_count", "input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "total_cost", "total_latency_ms", "min_latency_ms", "max_latency_ms", "latency_samples", "first_token_samples", "total_first_token_ms", "error_count", "fallback_count", "retry_count", "guardrail_blocks", "cache_hits", "distinct_sessions", "distinct_keys", "latency_bucket_50", "latency_bucket_100", "latency_bucket_200", "latency_bucket_500", "latency_bucket_1000", "latency_bucket_2000", "latency_bucket_5000", "latency_bucket_slow", datetime("created_at"), "ctx_system_tokens", "ctx_history_tokens", "ctx_question_tokens", "ctx_tool_tokens", "ctx_tool_output_tokens", "ctx_total_window", "ctx_analyzed_count", "ctx_overflow_count", "ctx_window_unknown_count", "ctx_util_bucket_lt50", "ctx_util_bucket_50_80", "ctx_util_bucket_80_95", "ctx_util_bucket_gt95"
FROM datalens_hourly_metrics;
DROP TABLE datalens_hourly_metrics;
ALTER TABLE datalens_hourly_metrics_rebuild RENAME TO datalens_hourly_metrics;
CREATE INDEX IF NOT EXISTS idx_dhm_key_hour      ON datalens_hourly_metrics (org_id, api_key_id, hour_bucket)
    WHERE api_key_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dhm_level_hour    ON datalens_hourly_metrics (org_id, agg_level, hour_bucket);
CREATE INDEX IF NOT EXISTS idx_dhm_model_hour    ON datalens_hourly_metrics (org_id, model_name, hour_bucket)
    WHERE model_name IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dhm_org_hour      ON datalens_hourly_metrics (org_id, hour_bucket);
CREATE INDEX IF NOT EXISTS idx_dhm_provider_hour ON datalens_hourly_metrics (org_id, provider_id, hour_bucket)
    WHERE provider_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dhm_team_hour     ON datalens_hourly_metrics (org_id, team_id, hour_bucket)
    WHERE team_id IS NOT NULL;

-- ==== datalens_partition_marker ====
CREATE TABLE datalens_partition_marker_rebuild (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    partitioned  INTEGER NOT NULL DEFAULT 0,
    migrated_at DATETIME,
    note         TEXT
);
INSERT INTO datalens_partition_marker_rebuild ("id", "partitioned", "migrated_at", "note")
SELECT "id", "partitioned", datetime("migrated_at"), "note"
FROM datalens_partition_marker;
DROP TABLE datalens_partition_marker;
ALTER TABLE datalens_partition_marker_rebuild RENAME TO datalens_partition_marker;

-- ==== datalens_reports ====
CREATE TABLE datalens_reports_rebuild (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id      INTEGER NOT NULL,
    user_id     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    description TEXT,
    type        TEXT NOT NULL DEFAULT 'custom',
    template_id TEXT,
    scope       TEXT NOT NULL DEFAULT 'private',
    config      TEXT NOT NULL,
    is_pinned   INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO datalens_reports_rebuild ("id", "org_id", "user_id", "name", "description", "type", "template_id", "scope", "config", "is_pinned", "version", "created_at", "updated_at", "deleted_at")
SELECT "id", "org_id", "user_id", "name", "description", "type", "template_id", "scope", "config", "is_pinned", "version", datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM datalens_reports;
DROP TABLE datalens_reports;
ALTER TABLE datalens_reports_rebuild RENAME TO datalens_reports;
CREATE INDEX IF NOT EXISTS idx_dr_deleted  ON datalens_reports (deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dr_org_user ON datalens_reports (org_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_dr_template ON datalens_reports (org_id, template_id) WHERE deleted_at IS NULL;

-- ==== datalens_schedules ====
CREATE TABLE datalens_schedules_rebuild (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id      INTEGER NOT NULL,
    user_id     INTEGER NOT NULL,
    team_id     INTEGER,
    name        TEXT NOT NULL,
    report_id   INTEGER NOT NULL,
    cron        TEXT NOT NULL,
    timezone    TEXT NOT NULL DEFAULT 'Asia/Shanghai',
    channels    TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run_at DATETIME,
    next_run_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO datalens_schedules_rebuild ("id", "org_id", "user_id", "team_id", "name", "report_id", "cron", "timezone", "channels", "enabled", "last_run_at", "next_run_at", "created_at", "updated_at", "deleted_at")
SELECT "id", "org_id", "user_id", "team_id", "name", "report_id", "cron", "timezone", "channels", "enabled", datetime("last_run_at"), datetime("next_run_at"), datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM datalens_schedules;
DROP TABLE datalens_schedules;
ALTER TABLE datalens_schedules_rebuild RENAME TO datalens_schedules;
CREATE INDEX IF NOT EXISTS idx_ds_org ON datalens_schedules (org_id, enabled, next_run_at) WHERE deleted_at IS NULL;

-- ==== error_classification_rules ====
CREATE TABLE error_classification_rules_rebuild (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    match_field    VARCHAR(16) NOT NULL,
    pattern        VARCHAR(128) NOT NULL,
    classification VARCHAR(16) NOT NULL DEFAULT 'quota',
    provider_type  VARCHAR(32),
    scope          VARCHAR(16) NOT NULL DEFAULT 'account',
    priority       INTEGER NOT NULL DEFAULT 100,
    enabled        INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO error_classification_rules_rebuild ("id", "match_field", "pattern", "classification", "provider_type", "scope", "priority", "enabled", "created_at", "updated_at")
SELECT "id", "match_field", "pattern", "classification", "provider_type", "scope", "priority", "enabled", datetime("created_at"), datetime("updated_at")
FROM error_classification_rules;
DROP TABLE error_classification_rules;
ALTER TABLE error_classification_rules_rebuild RENAME TO error_classification_rules;
CREATE INDEX IF NOT EXISTS idx_error_rules_enabled ON error_classification_rules(enabled);

-- ==== guardrail_alert_logs ====
CREATE TABLE guardrail_alert_logs_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id         INTEGER NOT NULL,
    alert_rule_id   INTEGER,
    rule_name       TEXT,
    engine_type     TEXT,
    severity        TEXT,
    action          TEXT,
    direction       TEXT,
    reason          TEXT,
    model           TEXT,
    content_preview TEXT,
    api_key_id      INTEGER,
    team_id         INTEGER,
    channels        TEXT,
    status          TEXT,
    agent_type      TEXT,
    org_id          INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
, rule_version INTEGER NOT NULL DEFAULT 0, matched TEXT);
INSERT INTO guardrail_alert_logs_rebuild ("id", "rule_id", "alert_rule_id", "rule_name", "engine_type", "severity", "action", "direction", "reason", "model", "content_preview", "api_key_id", "team_id", "channels", "status", "agent_type", "org_id", "created_at", "rule_version", "matched")
SELECT "id", "rule_id", "alert_rule_id", "rule_name", "engine_type", "severity", "action", "direction", "reason", "model", "content_preview", "api_key_id", "team_id", "channels", "status", "agent_type", "org_id", datetime("created_at"), "rule_version", "matched"
FROM guardrail_alert_logs;
DROP TABLE guardrail_alert_logs;
ALTER TABLE guardrail_alert_logs_rebuild RENAME TO guardrail_alert_logs;
CREATE INDEX IF NOT EXISTS idx_alert_logs_action ON guardrail_alert_logs(action);
CREATE INDEX IF NOT EXISTS idx_alert_logs_created_at ON guardrail_alert_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_alert_logs_rule_id ON guardrail_alert_logs(rule_id);
CREATE INDEX IF NOT EXISTS idx_alert_logs_severity ON guardrail_alert_logs(severity);
CREATE INDEX IF NOT EXISTS idx_alert_logs_team_id ON guardrail_alert_logs(team_id);
CREATE INDEX IF NOT EXISTS idx_guardrail_alert_logs_org_created ON guardrail_alert_logs (org_id, created_at);

-- ==== guardrail_alert_rules ====
CREATE TABLE guardrail_alert_rules_rebuild (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id           INTEGER NOT NULL REFERENCES guardrail_rules(id) ON DELETE CASCADE,
    team_id           INTEGER,
    channels          TEXT NOT NULL DEFAULT '[]',
    cooldown_minutes  INTEGER NOT NULL DEFAULT 5,
    enabled           INTEGER NOT NULL DEFAULT 1,
    last_triggered_at DATETIME,
    org_id            INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    UNIQUE (rule_id)
);
INSERT INTO guardrail_alert_rules_rebuild ("id", "rule_id", "team_id", "channels", "cooldown_minutes", "enabled", "last_triggered_at", "org_id", "created_at", "updated_at")
SELECT "id", "rule_id", "team_id", "channels", "cooldown_minutes", "enabled", datetime("last_triggered_at"), "org_id", datetime("created_at"), datetime("updated_at")
FROM guardrail_alert_rules;
DROP TABLE guardrail_alert_rules;
ALTER TABLE guardrail_alert_rules_rebuild RENAME TO guardrail_alert_rules;
CREATE INDEX IF NOT EXISTS idx_alert_rules_team_id ON guardrail_alert_rules(team_id);

-- ==== insights ====
CREATE TABLE insights_rebuild (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    period       TEXT NOT NULL DEFAULT 'monthly',
    period_key   TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT 'global',
    scope_id     INTEGER NOT NULL DEFAULT 0,
    insight_type TEXT NOT NULL DEFAULT 'summary',
    title        TEXT NOT NULL DEFAULT '',
    content      TEXT NOT NULL,
    model_used   TEXT NOT NULL DEFAULT '',
    tokens_used  INTEGER NOT NULL DEFAULT 0,
    org_id       INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO insights_rebuild ("id", "period", "period_key", "scope", "scope_id", "insight_type", "title", "content", "model_used", "tokens_used", "org_id", "created_at")
SELECT "id", "period", "period_key", "scope", "scope_id", "insight_type", "title", "content", "model_used", "tokens_used", "org_id", datetime("created_at")
FROM insights;
DROP TABLE insights;
ALTER TABLE insights_rebuild RENAME TO insights;
CREATE INDEX IF NOT EXISTS idx_insights_org_id ON insights(org_id);
CREATE INDEX IF NOT EXISTS idx_insights_period ON insights(period_key DESC);
CREATE INDEX IF NOT EXISTS idx_insights_scope ON insights(scope, scope_id);
CREATE INDEX IF NOT EXISTS idx_insights_type ON insights(insight_type);
CREATE UNIQUE INDEX IF NOT EXISTS idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);

-- ==== mcp_server_permissions ====
CREATE TABLE mcp_server_permissions_rebuild (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id      INTEGER NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    principal_type TEXT NOT NULL,
    principal_id   INTEGER NOT NULL,
    allow_tools    TEXT DEFAULT '["*"]',
    deny_tools     TEXT DEFAULT '[]',
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    UNIQUE (server_id, principal_type, principal_id)
);
INSERT INTO mcp_server_permissions_rebuild ("id", "server_id", "principal_type", "principal_id", "allow_tools", "deny_tools", "created_at")
SELECT "id", "server_id", "principal_type", "principal_id", "allow_tools", "deny_tools", datetime("created_at")
FROM mcp_server_permissions;
DROP TABLE mcp_server_permissions;
ALTER TABLE mcp_server_permissions_rebuild RENAME TO mcp_server_permissions;
CREATE INDEX IF NOT EXISTS idx_mcp_perm_principal ON mcp_server_permissions(principal_type, principal_id);

-- ==== mcp_servers ====
CREATE TABLE mcp_servers_rebuild (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT NOT NULL UNIQUE,
    display_name      TEXT,
    description       TEXT,
    transport_type    TEXT NOT NULL,
    url               TEXT,
    stdio_config      TEXT,
    auth_type         TEXT DEFAULT 'none',
    auth_config       TEXT,
    custom_headers    TEXT DEFAULT '{}',
    status            INTEGER NOT NULL DEFAULT 1,
    health_status     INTEGER NOT NULL DEFAULT 0,
    last_health_check TEXT,
    tool_count        INTEGER DEFAULT 0,
    enabled           INTEGER DEFAULT 1,
    tier_required     TEXT DEFAULT 'community',
    created_by        INTEGER,
    org_id            INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO mcp_servers_rebuild ("id", "name", "display_name", "description", "transport_type", "url", "stdio_config", "auth_type", "auth_config", "custom_headers", "status", "health_status", "last_health_check", "tool_count", "enabled", "tier_required", "created_by", "org_id", "created_at", "updated_at", "deleted_at")
SELECT "id", "name", "display_name", "description", "transport_type", "url", "stdio_config", "auth_type", "auth_config", "custom_headers", "status", "health_status", "last_health_check", "tool_count", "enabled", "tier_required", "created_by", "org_id", datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM mcp_servers;
DROP TABLE mcp_servers;
ALTER TABLE mcp_servers_rebuild RENAME TO mcp_servers;
CREATE INDEX IF NOT EXISTS idx_mcp_servers_deleted ON mcp_servers(deleted_at);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_enabled ON mcp_servers(enabled);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_org_id ON mcp_servers(org_id);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_status ON mcp_servers(status);

-- ==== mcp_tool_call_logs ====
CREATE TABLE mcp_tool_call_logs_rebuild (
    id           INTEGER,
    request_id   TEXT,
    server_id    INTEGER,
    server_name  TEXT,
    tool_name    TEXT,
    method       TEXT,
    input_size   INTEGER DEFAULT 0,
    output_size  INTEGER DEFAULT 0,
    duration     INTEGER DEFAULT 0,
    status       INTEGER NOT NULL,
    error_code   INTEGER,
    error_msg    TEXT,
    api_key_id   INTEGER,
    user_id      INTEGER,
    team_id      INTEGER,
    blocked_by   TEXT,
    org_id       INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (id, created_at)
);
INSERT INTO mcp_tool_call_logs_rebuild ("id", "request_id", "server_id", "server_name", "tool_name", "method", "input_size", "output_size", "duration", "status", "error_code", "error_msg", "api_key_id", "user_id", "team_id", "blocked_by", "org_id", "created_at")
SELECT "id", "request_id", "server_id", "server_name", "tool_name", "method", "input_size", "output_size", "duration", "status", "error_code", "error_msg", "api_key_id", "user_id", "team_id", "blocked_by", "org_id", datetime("created_at")
FROM mcp_tool_call_logs;
DROP TABLE mcp_tool_call_logs;
ALTER TABLE mcp_tool_call_logs_rebuild RENAME TO mcp_tool_call_logs;
CREATE INDEX IF NOT EXISTS idx_mcp_logs_key ON mcp_tool_call_logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_org_id ON mcp_tool_call_logs(org_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_server ON mcp_tool_call_logs(server_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_team ON mcp_tool_call_logs(team_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_time ON mcp_tool_call_logs(created_at);

-- ==== mcp_tool_call_logs_archive ====
CREATE TABLE mcp_tool_call_logs_archive_rebuild (
    id           INTEGER PRIMARY KEY,
    request_id   TEXT,
    server_id    INTEGER,
    server_name  TEXT,
    tool_name    TEXT,
    method       TEXT,
    input_size   INTEGER DEFAULT 0,
    output_size  INTEGER DEFAULT 0,
    duration     INTEGER DEFAULT 0,
    status       INTEGER NOT NULL,
    error_code   INTEGER,
    error_msg    TEXT,
    api_key_id   INTEGER,
    user_id      INTEGER,
    team_id      INTEGER,
    blocked_by   TEXT,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO mcp_tool_call_logs_archive_rebuild ("id", "request_id", "server_id", "server_name", "tool_name", "method", "input_size", "output_size", "duration", "status", "error_code", "error_msg", "api_key_id", "user_id", "team_id", "blocked_by", "created_at")
SELECT "id", "request_id", "server_id", "server_name", "tool_name", "method", "input_size", "output_size", "duration", "status", "error_code", "error_msg", "api_key_id", "user_id", "team_id", "blocked_by", datetime("created_at")
FROM mcp_tool_call_logs_archive;
DROP TABLE mcp_tool_call_logs_archive;
ALTER TABLE mcp_tool_call_logs_archive_rebuild RENAME TO mcp_tool_call_logs_archive;

-- ==== optimization_actions ====
CREATE TABLE optimization_actions_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    action_type     TEXT NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    priority        TEXT NOT NULL DEFAULT 'medium',
    status          TEXT NOT NULL DEFAULT 'pending',
    payload         TEXT NOT NULL DEFAULT '{}',
    saving_estimate REAL DEFAULT 0,
    org_id          INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    applied_at DATETIME,
    applied_by      INTEGER,
    dismissed_at DATETIME,
    dismissed_by    INTEGER
);
INSERT INTO optimization_actions_rebuild ("id", "action_type", "title", "description", "priority", "status", "payload", "saving_estimate", "org_id", "created_at", "applied_at", "applied_by", "dismissed_at", "dismissed_by")
SELECT "id", "action_type", "title", "description", "priority", "status", "payload", "saving_estimate", "org_id", datetime("created_at"), datetime("applied_at"), "applied_by", datetime("dismissed_at"), "dismissed_by"
FROM optimization_actions;
DROP TABLE optimization_actions;
ALTER TABLE optimization_actions_rebuild RENAME TO optimization_actions;
CREATE INDEX IF NOT EXISTS idx_opt_actions_created ON optimization_actions(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_opt_actions_priority ON optimization_actions(priority);
CREATE INDEX IF NOT EXISTS idx_opt_actions_status ON optimization_actions(status);
CREATE INDEX IF NOT EXISTS idx_opt_actions_type ON optimization_actions(action_type);
CREATE INDEX IF NOT EXISTS idx_optimization_actions_org_id ON optimization_actions(org_id);

-- ==== organization_members ====
CREATE TABLE organization_members_rebuild (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id     INTEGER NOT NULL REFERENCES organizations(id),
    user_id    INTEGER NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL,
    joined_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME,
    UNIQUE (org_id, user_id)
);
INSERT INTO organization_members_rebuild ("id", "org_id", "user_id", "role", "joined_at", "deleted_at")
SELECT "id", "org_id", "user_id", "role", datetime("joined_at"), datetime("deleted_at")
FROM organization_members;
DROP TABLE organization_members;
ALTER TABLE organization_members_rebuild RENAME TO organization_members;
CREATE INDEX IF NOT EXISTS idx_org_members_org_id ON organization_members(org_id);
CREATE INDEX IF NOT EXISTS idx_org_members_user_id ON organization_members(user_id);

-- ==== organizations ====
CREATE TABLE organizations_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    display_name    TEXT NOT NULL,
    description     TEXT,
    status          INTEGER NOT NULL DEFAULT 1,
    budget_limit    REAL NOT NULL DEFAULT 0,
    budget_period   TEXT NOT NULL DEFAULT 'monthly',
    rpm_limit       INTEGER NOT NULL DEFAULT 0,
    tpm_limit       INTEGER NOT NULL DEFAULT 0,
    settings        TEXT,
    created_by_id   INTEGER REFERENCES users(id),
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO organizations_rebuild ("id", "name", "display_name", "description", "status", "budget_limit", "budget_period", "rpm_limit", "tpm_limit", "settings", "created_by_id", "created_at", "updated_at", "deleted_at")
SELECT "id", "name", "display_name", "description", "status", "budget_limit", "budget_period", "rpm_limit", "tpm_limit", "settings", "created_by_id", datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM organizations;
DROP TABLE organizations;
ALTER TABLE organizations_rebuild RENAME TO organizations;
CREATE UNIQUE INDEX IF NOT EXISTS organizations_name_active_idx
    ON organizations(name) WHERE deleted_at IS NULL;

-- ==== provider_models ====
CREATE TABLE provider_models_rebuild (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id      INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_name       TEXT NOT NULL,
    provider_model   TEXT NOT NULL,
    weight           INTEGER NOT NULL DEFAULT 0,
    priority         INTEGER NOT NULL DEFAULT 1,
    status           INTEGER NOT NULL DEFAULT 1,
    max_context      INTEGER,
    input_price      REAL NOT NULL DEFAULT 0,
    output_price     REAL NOT NULL DEFAULT 0,
    currency         TEXT NOT NULL DEFAULT 'CNY',
    routing_strategy TEXT NOT NULL DEFAULT 'weighted_random',
    extra_config     TEXT,
    org_id           INTEGER REFERENCES organizations(id),
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO provider_models_rebuild ("id", "provider_id", "model_name", "provider_model", "weight", "priority", "status", "max_context", "input_price", "output_price", "currency", "routing_strategy", "extra_config", "org_id", "created_at", "updated_at", "deleted_at")
SELECT "id", "provider_id", "model_name", "provider_model", "weight", "priority", "status", "max_context", "input_price", "output_price", "currency", "routing_strategy", "extra_config", "org_id", datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM provider_models;
DROP TABLE provider_models;
ALTER TABLE provider_models_rebuild RENAME TO provider_models;
CREATE INDEX IF NOT EXISTS idx_provider_models_deleted_at ON provider_models(deleted_at);
CREATE INDEX IF NOT EXISTS idx_provider_models_model_status ON provider_models(model_name, status);
CREATE UNIQUE INDEX IF NOT EXISTS provider_models_active_unique
    ON provider_models(provider_id, model_name, provider_model) WHERE deleted_at IS NULL;

-- ==== providers ====
CREATE TABLE providers_rebuild (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT UNIQUE NOT NULL,
    display_name  TEXT NOT NULL,
    adapter_type  TEXT NOT NULL,
    base_url      TEXT NOT NULL,
    api_key       TEXT NOT NULL,
    extra_config  TEXT,
    status        INTEGER NOT NULL DEFAULT 1,
    org_id        INTEGER REFERENCES organizations(id),
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO providers_rebuild ("id", "name", "display_name", "adapter_type", "base_url", "api_key", "extra_config", "status", "org_id", "created_at", "updated_at", "deleted_at")
SELECT "id", "name", "display_name", "adapter_type", "base_url", "api_key", "extra_config", "status", "org_id", datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM providers;
DROP TABLE providers;
ALTER TABLE providers_rebuild RENAME TO providers;
CREATE INDEX IF NOT EXISTS idx_providers_deleted_at ON providers(deleted_at);
CREATE INDEX IF NOT EXISTS idx_providers_org_id ON providers(org_id);

-- ==== roles ====
CREATE TABLE roles_rebuild (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL,
    is_system    INTEGER NOT NULL DEFAULT 0,
    org_id       INTEGER,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO roles_rebuild ("id", "name", "display_name", "is_system", "org_id", "created_at", "deleted_at")
SELECT "id", "name", "display_name", "is_system", "org_id", datetime("created_at"), datetime("deleted_at")
FROM roles;
DROP TABLE roles;
ALTER TABLE roles_rebuild RENAME TO roles;
CREATE INDEX IF NOT EXISTS idx_roles_deleted_at ON roles(deleted_at);

-- ==== system_settings ====
CREATE TABLE system_settings_rebuild (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO system_settings_rebuild ("key", "value", "updated_at")
SELECT "key", "value", datetime("updated_at")
FROM system_settings;
DROP TABLE system_settings;
ALTER TABLE system_settings_rebuild RENAME TO system_settings;

-- ==== team_members ====
CREATE TABLE team_members_rebuild (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member',
    joined_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME,
    UNIQUE (team_id, user_id)
);
INSERT INTO team_members_rebuild ("id", "team_id", "user_id", "role", "joined_at", "deleted_at")
SELECT "id", "team_id", "user_id", "role", datetime("joined_at"), datetime("deleted_at")
FROM team_members;
DROP TABLE team_members;
ALTER TABLE team_members_rebuild RENAME TO team_members;
CREATE INDEX IF NOT EXISTS idx_team_members_deleted_at ON team_members(deleted_at);
CREATE INDEX IF NOT EXISTS idx_team_members_user_id ON team_members(user_id);

-- ==== teams ====
CREATE TABLE teams_rebuild (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT UNIQUE NOT NULL,
    display_name    TEXT NOT NULL,
    description     TEXT,
    budget_limit    REAL NOT NULL DEFAULT 0,
    budget_period   TEXT NOT NULL DEFAULT 'monthly',
    rpm_limit       INTEGER NOT NULL DEFAULT 0,
    tpm_limit       INTEGER NOT NULL DEFAULT 0,
    status          INTEGER NOT NULL DEFAULT 1,
    org_id          INTEGER NOT NULL REFERENCES organizations(id),
    created_by_id   INTEGER NOT NULL REFERENCES users(id),
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
);
INSERT INTO teams_rebuild ("id", "name", "display_name", "description", "budget_limit", "budget_period", "rpm_limit", "tpm_limit", "status", "org_id", "created_by_id", "created_at", "updated_at", "deleted_at")
SELECT "id", "name", "display_name", "description", "budget_limit", "budget_period", "rpm_limit", "tpm_limit", "status", "org_id", "created_by_id", datetime("created_at"), datetime("updated_at"), datetime("deleted_at")
FROM teams;
DROP TABLE teams;
ALTER TABLE teams_rebuild RENAME TO teams;
CREATE INDEX IF NOT EXISTS idx_teams_created_by_id ON teams(created_by_id);
CREATE INDEX IF NOT EXISTS idx_teams_deleted_at ON teams(deleted_at);
CREATE INDEX IF NOT EXISTS idx_teams_org_id ON teams(org_id);

-- ==== users ====
CREATE TABLE users_rebuild (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    username              TEXT UNIQUE NOT NULL,
    password_hash         TEXT NOT NULL,
    display_name          TEXT NOT NULL,
    role_id               INTEGER NOT NULL REFERENCES roles(id),
    status                INTEGER NOT NULL DEFAULT 1,
    email                 TEXT,
    force_password_change INTEGER NOT NULL DEFAULT 0,
    preferences           TEXT,
    org_id                INTEGER,
    last_login_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at DATETIME
, sso_provider_id INTEGER, sso_id TEXT DEFAULT '');
INSERT INTO users_rebuild ("id", "username", "password_hash", "display_name", "role_id", "status", "email", "force_password_change", "preferences", "org_id", "last_login_at", "created_at", "updated_at", "deleted_at", "sso_provider_id", "sso_id")
SELECT "id", "username", "password_hash", "display_name", "role_id", "status", "email", "force_password_change", "preferences", "org_id", datetime("last_login_at"), datetime("created_at"), datetime("updated_at"), datetime("deleted_at"), "sso_provider_id", "sso_id"
FROM users;
DROP TABLE users;
ALTER TABLE users_rebuild RENAME TO users;
CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users(deleted_at);
CREATE INDEX IF NOT EXISTS idx_users_role_id ON users(role_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_sso
    ON users(sso_provider_id, sso_id)
    WHERE sso_id IS NOT NULL AND sso_id != '' AND deleted_at IS NULL;
PRAGMA foreign_keys = ON;
