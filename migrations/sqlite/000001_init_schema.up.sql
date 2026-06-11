-- SQLite full schema for CrossLink Community Edition.
-- Translated from PostgreSQL terminal schema (52 migrations applied).
-- Tables are ordered by foreign key dependency (referenced tables first).

-- ============================================================
-- PRAGMA: enable foreign key enforcement for this connection.
-- GORM/DNS should also set _pragma=foreign_keys(1), but this
-- ensures raw migration execution is safe.
-- ============================================================
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;

-- ============================================================
-- 1. system_settings — key/value store, no FK dependencies
-- ============================================================
CREATE TABLE system_settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ============================================================
-- 2. roles — RBAC role definitions
--    Column source: 000012 (base), 000036 (deleted_at), 000047 (org_id)
-- ============================================================
CREATE TABLE roles (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL,
    is_system    INTEGER NOT NULL DEFAULT 0,
    org_id       INTEGER,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_roles_deleted_at ON roles(deleted_at);

-- ============================================================
-- 3. users
--    Column source: 000009 (base), 000012 (role_id, drop role), 000036 (deleted_at),
--                   000044 (preferences), 000047 (org_id), 000050 (email, force_password_change)
-- ============================================================
CREATE TABLE users (
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
    last_login_at         TEXT,
    created_at            TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at            TEXT
);

CREATE INDEX IF NOT EXISTS idx_users_role_id ON users(role_id);
CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users(deleted_at);

-- ============================================================
-- 4. organizations
--    Column source: 000047 (base)
--    Note: plain UNIQUE on name dropped (000051); replaced by partial unique index below.
-- ============================================================
CREATE TABLE organizations (
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
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at      TEXT
);

-- Partial unique index: only enforce uniqueness on non-deleted org names
CREATE UNIQUE INDEX IF NOT EXISTS organizations_name_active_idx
    ON organizations(name) WHERE deleted_at IS NULL;

-- ============================================================
-- 5. organization_members
--    Column source: 000047 (base)
-- ============================================================
CREATE TABLE organization_members (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id     INTEGER NOT NULL REFERENCES organizations(id),
    user_id    INTEGER NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL,
    joined_at  TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at TEXT,
    UNIQUE (org_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_org_members_org_id ON organization_members(org_id);
CREATE INDEX IF NOT EXISTS idx_org_members_user_id ON organization_members(user_id);

-- ============================================================
-- 6. teams
--    Column source: 000009 (base), 000036 (deleted_at), 000047 (org_id + NOT NULL)
-- ============================================================
CREATE TABLE teams (
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
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_teams_created_by_id ON teams(created_by_id);
CREATE INDEX IF NOT EXISTS idx_teams_deleted_at ON teams(deleted_at);
CREATE INDEX IF NOT EXISTS idx_teams_org_id ON teams(org_id);

-- ============================================================
-- 7. team_members
--    Column source: 000009 (base), 000036 (deleted_at)
-- ============================================================
CREATE TABLE team_members (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member',
    joined_at  TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at TEXT,
    UNIQUE (team_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_team_members_user_id ON team_members(user_id);
CREATE INDEX IF NOT EXISTS idx_team_members_deleted_at ON team_members(deleted_at);

-- ============================================================
-- 8. role_permissions
--    Column source: 000012 (base)
-- ============================================================
CREATE TABLE role_permissions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    action  TEXT NOT NULL,
    UNIQUE (role_id, action)
);

-- ============================================================
-- 9. providers
--    Column source: 000001 (base), 000003 (CHECK constraint — omitted for SQLite),
--                   000036 (deleted_at), 000047 (org_id)
-- ============================================================
CREATE TABLE providers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT UNIQUE NOT NULL,
    display_name  TEXT NOT NULL,
    adapter_type  TEXT NOT NULL,
    base_url      TEXT NOT NULL,
    api_key       TEXT NOT NULL,
    extra_config  TEXT,
    status        INTEGER NOT NULL DEFAULT 1,
    org_id        INTEGER REFERENCES organizations(id),
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_providers_deleted_at ON providers(deleted_at);
CREATE INDEX IF NOT EXISTS idx_providers_org_id ON providers(org_id);

-- ============================================================
-- 10. provider_models
--    Column source: 000001 (base), 000004 (currency), 000013 (routing_strategy),
--                   000014 (CHECK update — omitted for SQLite),
--                   000036 (deleted_at), 000047 (org_id)
--    Note: original UNIQUE(provider_id,model_name,provider_model) replaced by
--    partial unique index (000045) that only applies to non-deleted rows.
-- ============================================================
CREATE TABLE provider_models (
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
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at       TEXT
);

-- Partial unique index: enforce uniqueness only on non-deleted rows
CREATE UNIQUE INDEX IF NOT EXISTS provider_models_active_unique
    ON provider_models(provider_id, model_name, provider_model) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_provider_models_deleted_at ON provider_models(deleted_at);
CREATE INDEX IF NOT EXISTS idx_provider_models_model_status ON provider_models(model_name, status);

-- ============================================================
-- 11. api_keys
--    Column source: 000001 (base), 000009 (created_by_id, team_id), 000011 (max_budget, budget_period),
--                   000036 (deleted_at), 000046 (email), 000047 (org_id), 000052 (max_calls, call_period)
-- ============================================================
CREATE TABLE api_keys (
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
    expires_at          TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at        TEXT,
    deleted_at          TEXT
);

CREATE INDEX IF NOT EXISTS idx_api_keys_created_by_id ON api_keys(created_by_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_team_id ON api_keys(team_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_deleted_at ON api_keys(deleted_at);
CREATE INDEX IF NOT EXISTS idx_api_keys_org_id ON api_keys(org_id);

-- ============================================================
-- 12. api_key_hashes
--    Column source: 000007 (base), 000008 (indexes), 000041 (hash_algo)
-- ============================================================
CREATE TABLE api_key_hashes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id  INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    key_hash    TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    hash_algo   TEXT NOT NULL DEFAULT 'sha256',
    is_primary  INTEGER NOT NULL DEFAULT 1,
    grace_until TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_key_hashes_key_hash ON api_key_hashes(key_hash);
CREATE INDEX IF NOT EXISTS idx_api_key_hashes_api_key_id ON api_key_hashes(api_key_id);
CREATE INDEX IF NOT EXISTS idx_api_key_hashes_grace_until ON api_key_hashes(grace_until) WHERE grace_until IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_key_hashes_one_primary ON api_key_hashes(api_key_id) WHERE is_primary = 1;

-- ============================================================
-- 13. usage_logs
--    Column source: 000001 (base), 000002 (user_message, model_response), 000005 (currency),
--                   000006 (cost precision — REAL in SQLite), 000009 (team_id),
--                   000015 (fallback_count), 000016 (retry_count),
--                   000019 (guardrail_triggered, guardrail_rule),
--                   000020 (cache_hit), 000034 (agent_type, security_events),
--                   000047 (org_id)
-- ============================================================
CREATE TABLE usage_logs (
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
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    first_token_ms    INTEGER,
    status_code       INTEGER NOT NULL,
    error_type        TEXT,
    currency          TEXT NOT NULL DEFAULT 'CNY',
    team_id           INTEGER REFERENCES teams(id) ON DELETE SET NULL,
    user_message      TEXT,
    model_response    TEXT,
    fallback_count    INTEGER NOT NULL DEFAULT 0,
    retry_count       INTEGER NOT NULL DEFAULT 0,
    guardrail_triggered INTEGER DEFAULT 0,
    guardrail_rule    TEXT,
    cache_hit         INTEGER NOT NULL DEFAULT 0,
    agent_type        TEXT,
    security_events   TEXT DEFAULT '[]',
    org_id            INTEGER REFERENCES organizations(id),
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    session_id        TEXT,
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

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

-- ============================================================
-- 14. guardrail_rules
--    Column source: 000018 (base), 000047 (org_id)
-- ============================================================
CREATE TABLE guardrail_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL,
    direction  TEXT NOT NULL,
    enabled    INTEGER DEFAULT 1,
    config     TEXT NOT NULL,
    severity   TEXT DEFAULT 'medium',
    action     TEXT DEFAULT 'block',
    model_filter TEXT,
    org_id     INTEGER,
    created_at TEXT DEFAULT (datetime('now')),
    updated_at TEXT DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_guardrail_rules_type ON guardrail_rules(type);
CREATE INDEX IF NOT EXISTS idx_guardrail_rules_enabled ON guardrail_rules(enabled) WHERE enabled = 1;

-- ============================================================
-- 15. guardrail_alert_rules
--    Column source: 000026 (base), 000028 (TIMESTAMPTZ fix — already TEXT in SQLite),
--                   000047 (org_id)
-- ============================================================
CREATE TABLE guardrail_alert_rules (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id           INTEGER NOT NULL REFERENCES guardrail_rules(id) ON DELETE CASCADE,
    team_id           INTEGER,
    channels          TEXT NOT NULL DEFAULT '[]',
    cooldown_minutes  INTEGER NOT NULL DEFAULT 5,
    enabled           INTEGER NOT NULL DEFAULT 1,
    last_triggered_at TEXT,
    org_id            INTEGER,
    created_at        TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (rule_id)
);

CREATE INDEX IF NOT EXISTS idx_alert_rules_team_id ON guardrail_alert_rules(team_id);

-- ============================================================
-- 16. guardrail_alert_logs
--    Column source: 000026 (base), 000028 (TIMESTAMPTZ fix), 000029 (indexes),
--                   000034 (agent_type), 000047 (org_id)
-- ============================================================
CREATE TABLE guardrail_alert_logs (
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
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_alert_logs_rule_id ON guardrail_alert_logs(rule_id);
CREATE INDEX IF NOT EXISTS idx_alert_logs_created_at ON guardrail_alert_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_alert_logs_team_id ON guardrail_alert_logs(team_id);
CREATE INDEX IF NOT EXISTS idx_alert_logs_severity ON guardrail_alert_logs(severity);
CREATE INDEX IF NOT EXISTS idx_alert_logs_action ON guardrail_alert_logs(action);

-- ============================================================
-- 17. budget_alerts
--    Column source: 000011 (base), 000036 (deleted_at), 000047 (org_id)
-- ============================================================
CREATE TABLE budget_alerts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    team_id          INTEGER REFERENCES teams(id) ON DELETE CASCADE,
    key_id           INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    org_id           INTEGER,
    threshold_pct    INTEGER NOT NULL,
    webhook_url      TEXT NOT NULL,
    last_triggered_at TEXT,
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at       TEXT
);

CREATE INDEX IF NOT EXISTS idx_budget_alerts_team ON budget_alerts(team_id);
CREATE INDEX IF NOT EXISTS idx_budget_alerts_key ON budget_alerts(key_id);
CREATE INDEX IF NOT EXISTS idx_budget_alerts_deleted_at ON budget_alerts(deleted_at);

-- ============================================================
-- 18. budget_snapshots
--    Column source: 000011 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE budget_snapshots (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type TEXT NOT NULL,
    target_id   INTEGER NOT NULL,
    period_key  TEXT NOT NULL,
    spent       REAL NOT NULL DEFAULT 0,
    budget      REAL NOT NULL DEFAULT 0,
    currency    TEXT NOT NULL DEFAULT 'CNY',
    org_id      INTEGER,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (target_type, target_id, period_key)
);

CREATE INDEX IF NOT EXISTS idx_budget_snapshots_org_id ON budget_snapshots(org_id);

-- ============================================================
-- 19. budget_recommendations
--    Column source: 000030 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE budget_recommendations (
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
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_budget_recs_target ON budget_recommendations(target_type, target_id);
CREATE INDEX IF NOT EXISTS idx_budget_recs_created ON budget_recommendations(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_budget_recommendations_org_id ON budget_recommendations(org_id);

-- ============================================================
-- 20. budget_requests
--    Column source: 000030 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE budget_requests (
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
    reviewed_at        TEXT,
    org_id             INTEGER,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_budget_reqs_target ON budget_requests(target_type, target_id);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_status ON budget_requests(status);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_creator ON budget_requests(created_by);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_reviewer ON budget_requests(reviewed_by);
CREATE INDEX IF NOT EXISTS idx_budget_reqs_created ON budget_requests(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_budget_requests_org_id ON budget_requests(org_id);

-- ============================================================
-- 21. audit_logs
--    Column source: 000023 (base), 000024 (composite indexes), 000047 (org_id)
-- ============================================================
CREATE TABLE audit_logs (
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
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_user_id ON audit_logs(user_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs(action);
CREATE INDEX IF NOT EXISTS idx_audit_logs_resource ON audit_logs(resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action_created ON audit_logs(action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_resource_type_created ON audit_logs(resource_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_org_id ON audit_logs(org_id);

-- ============================================================
-- 22. insights
--    Column source: 000031 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE insights (
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
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_insights_period ON insights(period_key DESC);
CREATE INDEX IF NOT EXISTS idx_insights_scope ON insights(scope, scope_id);
CREATE INDEX IF NOT EXISTS idx_insights_type ON insights(insight_type);
CREATE UNIQUE INDEX IF NOT EXISTS idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);
CREATE INDEX IF NOT EXISTS idx_insights_org_id ON insights(org_id);

-- ============================================================
-- 23. optimization_actions
--    Column source: 000032 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE optimization_actions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    action_type     TEXT NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    priority        TEXT NOT NULL DEFAULT 'medium',
    status          TEXT NOT NULL DEFAULT 'pending',
    payload         TEXT NOT NULL DEFAULT '{}',
    saving_estimate REAL DEFAULT 0,
    org_id          INTEGER,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    applied_at      TEXT,
    applied_by      INTEGER,
    dismissed_at    TEXT,
    dismissed_by    INTEGER
);

CREATE INDEX IF NOT EXISTS idx_opt_actions_status ON optimization_actions(status);
CREATE INDEX IF NOT EXISTS idx_opt_actions_type ON optimization_actions(action_type);
CREATE INDEX IF NOT EXISTS idx_opt_actions_priority ON optimization_actions(priority);
CREATE INDEX IF NOT EXISTS idx_opt_actions_created ON optimization_actions(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_optimization_actions_org_id ON optimization_actions(org_id);

-- ============================================================
-- 24. agent_fingerprints
--    Column source: 000035 (base), 000047 (org_id)
-- ============================================================
CREATE TABLE agent_fingerprints (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    source_type     TEXT NOT NULL DEFAULT 'header',
    source_field    TEXT NOT NULL DEFAULT '',
    pattern         TEXT NOT NULL,
    risk_level      TEXT NOT NULL DEFAULT 'medium',
    origin          TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    hit_count       INTEGER NOT NULL DEFAULT 0,
    last_hit_at     TEXT,
    discovered_from TEXT,
    org_id          INTEGER,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_status ON agent_fingerprints(status);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_name ON agent_fingerprints(name);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_origin ON agent_fingerprints(origin);
CREATE INDEX IF NOT EXISTS idx_agent_fingerprints_status_type ON agent_fingerprints(status, source_type, source_field);

-- Dedup index: prevents duplicate fingerprints by source+field+pattern.
-- PG version uses md5(pattern) to bound index size; SQLite handles TEXT indexes natively.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_fingerprints_dedup
    ON agent_fingerprints(source_type, source_field, pattern);

-- ============================================================
-- 25. mcp_servers
--    Column source: 000038 (base), 000047 (not directly — added in 000048), 000048 (org_id)
--    CHECK constraints omitted for SQLite.
-- ============================================================
CREATE TABLE mcp_servers (
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
    created_at        TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at        TEXT
);

CREATE INDEX IF NOT EXISTS idx_mcp_servers_status ON mcp_servers(status);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_enabled ON mcp_servers(enabled);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_deleted ON mcp_servers(deleted_at);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_org_id ON mcp_servers(org_id);

-- ============================================================
-- 26. mcp_server_permissions
--    Column source: 000038 (base)
-- ============================================================
CREATE TABLE mcp_server_permissions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id      INTEGER NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    principal_type TEXT NOT NULL,
    principal_id   INTEGER NOT NULL,
    allow_tools    TEXT DEFAULT '["*"]',
    deny_tools     TEXT DEFAULT '[]',
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (server_id, principal_type, principal_id)
);

CREATE INDEX IF NOT EXISTS idx_mcp_perm_principal ON mcp_server_permissions(principal_type, principal_id);

-- ============================================================
-- 27. mcp_tool_call_logs
--    Column source: 000038 (base, partition parent — becomes plain table in SQLite),
--                   000048 (org_id)
--    Note: PG uses PARTITION BY RANGE with monthly partitions + default partition.
--    SQLite: single plain table. Composite PK (id, created_at) retained.
-- ============================================================
CREATE TABLE mcp_tool_call_logs (
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
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (id, created_at)
);

CREATE INDEX IF NOT EXISTS idx_mcp_logs_server ON mcp_tool_call_logs(server_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_key ON mcp_tool_call_logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_team ON mcp_tool_call_logs(team_id);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_time ON mcp_tool_call_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_mcp_logs_org_id ON mcp_tool_call_logs(org_id);

-- ============================================================
-- 28. mcp_tool_call_logs_archive
--    Archive table for aged-out MCP tool call logs.
--    Not in PG migrations — defined in the multi-database design doc.
-- ============================================================
CREATE TABLE mcp_tool_call_logs_archive (
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
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ============================================================
-- 29. datalens_hourly_metrics — DataLens hourly pre-aggregation
--    Column source: 000056 (base), 000058 (agg_type widen)
--    Note: PG COALESCE unique index omitted; SQLite handles TEXT
--    indexes natively, but expression indexes with COALESCE are
--    not straightforward for UPSERT. Use regular indexes instead.
-- ============================================================
CREATE TABLE datalens_hourly_metrics (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL,
    agg_level       TEXT NOT NULL,
    team_id         INTEGER,
    api_key_id      INTEGER,
    provider_id     INTEGER,
    model_name      TEXT,
    route_type      TEXT,
    status_group    INTEGER NOT NULL DEFAULT 200,
    hour_bucket     TEXT NOT NULL,
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

    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_dhm_org_hour      ON datalens_hourly_metrics (org_id, hour_bucket);
CREATE INDEX IF NOT EXISTS idx_dhm_level_hour    ON datalens_hourly_metrics (org_id, agg_level, hour_bucket);
CREATE INDEX IF NOT EXISTS idx_dhm_model_hour    ON datalens_hourly_metrics (org_id, model_name, hour_bucket)
    WHERE model_name IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dhm_team_hour     ON datalens_hourly_metrics (org_id, team_id, hour_bucket)
    WHERE team_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dhm_key_hour      ON datalens_hourly_metrics (org_id, api_key_id, hour_bucket)
    WHERE api_key_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dhm_provider_hour ON datalens_hourly_metrics (org_id, provider_id, hour_bucket)
    WHERE provider_id IS NOT NULL;

-- ============================================================
-- 30. datalens_daily_metrics — DataLens daily pre-aggregation
--    Column source: 000056 (base)
-- ============================================================
CREATE TABLE datalens_daily_metrics (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL,
    agg_level       TEXT NOT NULL,
    team_id         INTEGER,
    api_key_id      INTEGER,
    provider_id     INTEGER,
    model_name      TEXT,
    route_type      TEXT,
    status_group    INTEGER NOT NULL DEFAULT 200,
    day_bucket      TEXT NOT NULL,
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

    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_ddm_org_day ON datalens_daily_metrics (org_id, day_bucket);

-- ============================================================
-- 31. datalens_agg_status — DataLens aggregation health status
--    Column source: 000056 (base), 000058 (agg_type VARCHAR(32))
-- ============================================================
CREATE TABLE datalens_agg_status (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agg_level       TEXT NOT NULL,
    agg_type        TEXT NOT NULL,
    last_success_at TEXT NOT NULL,
    last_duration_ms INTEGER NOT NULL DEFAULT 0,
    rows_affected   INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_das_level_type ON datalens_agg_status (agg_level, agg_type);

-- ============================================================
-- 32. datalens_reports — DataLens saved report configurations
--    Column source: 000056 (base)
-- ============================================================
CREATE TABLE datalens_reports (
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
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_dr_org_user ON datalens_reports (org_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_dr_template ON datalens_reports (org_id, template_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_dr_deleted  ON datalens_reports (deleted_at) WHERE deleted_at IS NOT NULL;

-- ============================================================
-- 33. datalens_schedules — DataLens automated report schedules (Enterprise)
--    Column source: 000056 (base)
-- ============================================================
CREATE TABLE datalens_schedules (
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
    last_run_at TEXT,
    next_run_at TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_ds_org ON datalens_schedules (org_id, enabled, next_run_at) WHERE deleted_at IS NULL;

-- ============================================================
-- 34. datalens_partition_marker — DataLens partition migration tracking
--    Column source: 000057 (base)
-- ============================================================
CREATE TABLE datalens_partition_marker (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    partitioned  INTEGER NOT NULL DEFAULT 0,
    migrated_at  TEXT,
    note         TEXT
);

INSERT INTO datalens_partition_marker (partitioned, note) VALUES (0, 'Partition migration pending. Run: crosslink migrate-partition');
