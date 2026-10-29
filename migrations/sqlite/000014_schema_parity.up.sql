-- Schema parity catch-up: sqlite short chain (consolidated baseline) is
-- missing almost the same objects as mysql — everything only the postgres
-- chain received (pg 000054–000078). Diffed via PRAGMA table_info against a
-- fresh postgres-87 deployment; see mysql/000012_schema_parity for the method.
-- Scope differs from mysql in one point: api_keys.price_multiplier /
-- key_source / source_vendor were already folded into sqlite's consolidated
-- 000001 and are NOT re-added here.
--
-- pg-only artifacts (mcp_tool_call_logs_YYYY_MM partitions, _default) are
-- intentionally NOT replicated: sqlite uses mcp_tool_call_logs_archive.
-- SQLite supports partial indexes, so the soft-delete-aware uniques are
-- replicated verbatim from the postgres definitions.

-- ==== missing tables ====

-- VCR playback (pg 000069)
CREATE TABLE IF NOT EXISTS mock_fixtures (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_name TEXT NOT NULL,
    model         TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    response_body TEXT,
    stream_chunks TEXT,
    is_stream     INTEGER NOT NULL DEFAULT 0,
    created_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at    DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS mock_fixtures_lookup ON mock_fixtures (request_hash, model);
CREATE INDEX IF NOT EXISTS mock_fixtures_provider ON mock_fixtures (provider_name);

-- SSO identity providers + users SSO columns (pg 000054)
CREATE TABLE IF NOT EXISTS sso_providers (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT NOT NULL,
    type              TEXT NOT NULL DEFAULT 'oidc',
    issuer            TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    client_secret     TEXT NOT NULL,
    scopes            TEXT DEFAULT 'openid profile email',
    default_org_id    INTEGER REFERENCES organizations(id),
    default_role_id   INTEGER REFERENCES roles(id),
    auto_create       INTEGER NOT NULL DEFAULT 1,
    redirect_base_url TEXT,
    auth_url          TEXT,
    token_url         TEXT,
    userinfo_url      TEXT,
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at        DATETIME NOT NULL DEFAULT (datetime('now'))
);
ALTER TABLE users ADD COLUMN sso_provider_id INTEGER;
ALTER TABLE users ADD COLUMN sso_id TEXT DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_sso
    ON users(sso_provider_id, sso_id)
    WHERE sso_id IS NOT NULL AND sso_id != '' AND deleted_at IS NULL;

-- Scoped personal access tokens (pg 000074)
CREATE TABLE IF NOT EXISTS pat_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL,
    status       INTEGER NOT NULL DEFAULT 1,
    expires_at   DATETIME NOT NULL,
    last_used_at DATETIME,
    created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    revoked_at   DATETIME
);
CREATE INDEX IF NOT EXISTS idx_pat_tokens_user_id ON pat_tokens (user_id);

-- Prompt templates (pg 000068)
CREATE TABLE IF NOT EXISTS prompt_templates (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT NOT NULL,
    description      TEXT,
    system_prompt    TEXT NOT NULL DEFAULT '',
    variables_schema TEXT,
    few_shot         TEXT,
    tool_defs        TEXT,
    target_format    TEXT NOT NULL DEFAULT 'auto',
    status           INTEGER NOT NULL DEFAULT 1,
    version          INTEGER NOT NULL DEFAULT 1,
    org_id           INTEGER,
    created_at       DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at       DATETIME NOT NULL DEFAULT (datetime('now')),
    deleted_at       DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS prompt_templates_name_key
    ON prompt_templates (name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_prompt_templates_deleted_at ON prompt_templates (deleted_at);

-- Provider guard alert rules (pg 000066)
CREATE TABLE IF NOT EXISTS provider_guard_alert_rules (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id           INTEGER,
    provider_name    TEXT NOT NULL DEFAULT '',
    model_name       TEXT NOT NULL DEFAULT '',
    limit_type       TEXT NOT NULL DEFAULT '',
    channels         TEXT NOT NULL DEFAULT '[]',
    cooldown_seconds INTEGER NOT NULL DEFAULT 300,
    enabled          INTEGER NOT NULL DEFAULT 1,
    last_triggered_at DATETIME,
    created_at       DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at       DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_pgar_org_enabled ON provider_guard_alert_rules (org_id, enabled);

-- ==== missing columns ====

-- DataLens context-window metrics (pg 000073), hourly + daily
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_system_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_history_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_question_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_tool_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_tool_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_total_window INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_analyzed_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_overflow_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_window_unknown_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_util_bucket_lt50 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_util_bucket_50_80 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_util_bucket_80_95 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_hourly_metrics ADD COLUMN ctx_util_bucket_gt95 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_system_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_history_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_question_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_tool_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_tool_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_total_window INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_analyzed_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_overflow_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_window_unknown_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_util_bucket_lt50 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_util_bucket_50_80 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_util_bucket_80_95 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics ADD COLUMN ctx_util_bucket_gt95 INTEGER NOT NULL DEFAULT 0;

-- ==== missing seeds: system roles ====
INSERT INTO roles (name, display_name, is_system, created_at)
SELECT 'admin', 'Administrator', 1, datetime('now')
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = 'admin');
INSERT INTO roles (name, display_name, is_system, created_at)
SELECT 'member', 'Member', 1, datetime('now')
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = 'member');
INSERT INTO roles (name, display_name, is_system, created_at)
SELECT 'viewer', 'Viewer', 1, datetime('now')
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = 'viewer');
