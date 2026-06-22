-- MySQL full schema for CrossLink Community Edition.
-- Translated from PostgreSQL terminal schema (52 migrations applied).
-- Tables are ordered by foreign key dependency (referenced tables first).
-- Requires MySQL 8.0.16+ (CHECK constraints) and 8.0.13+ (JSON DEFAULT values).

SET NAMES utf8mb4;

-- ============================================================
-- 1. system_settings — key/value store, no FK dependencies
-- ============================================================
CREATE TABLE system_settings (
    `key`       VARCHAR(128) PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ============================================================
-- 2. roles — RBAC role definitions
--    Column source: 000012 (base), 000036 (deleted_at), 000047 (org_id)
-- ============================================================
CREATE TABLE roles (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    name         VARCHAR(32) UNIQUE NOT NULL,
    display_name VARCHAR(64) NOT NULL,
    is_system    TINYINT(1) NOT NULL DEFAULT 0,
    org_id       BIGINT,
    created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at   DATETIME(3) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_roles_deleted_at ON roles(deleted_at);

-- ============================================================
-- 3. users
--    Column source: 000009 (base), 000012 (role_id, drop role), 000036 (deleted_at),
--                   000044 (preferences), 000047 (org_id), 000050 (email, force_password_change)
-- ============================================================
CREATE TABLE users (
    id                    BIGINT AUTO_INCREMENT PRIMARY KEY,
    username              VARCHAR(64) UNIQUE NOT NULL,
    password_hash         VARCHAR(128) NOT NULL,
    display_name          VARCHAR(128) NOT NULL,
    role_id               BIGINT NOT NULL,
    status                SMALLINT NOT NULL DEFAULT 1,
    email                 VARCHAR(255),
    force_password_change TINYINT(1) NOT NULL DEFAULT 0,
    preferences           JSON,
    org_id                BIGINT,
    last_login_at         DATETIME(3) NULL,
    created_at            DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at            DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at            DATETIME(3) NULL,
    CONSTRAINT fk_users_role_id FOREIGN KEY (role_id) REFERENCES roles(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_users_role_id ON users(role_id);
CREATE INDEX idx_users_deleted_at ON users(deleted_at);

-- ============================================================
-- 4. organizations
--    Column source: 000047 (base)
--    Note: plain UNIQUE on name dropped (000051); replaced by partial unique index.
--    MySQL has no partial indexes, so we use a regular unique index.
-- ============================================================
CREATE TABLE organizations (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    name            VARCHAR(64) NOT NULL,
    display_name    VARCHAR(128) NOT NULL,
    description     TEXT,
    status          SMALLINT NOT NULL DEFAULT 1,
    budget_limit    DECIMAL(12,2) NOT NULL DEFAULT 0,
    budget_period   VARCHAR(16) NOT NULL DEFAULT 'monthly',
    rpm_limit       INT NOT NULL DEFAULT 0,
    tpm_limit       INT NOT NULL DEFAULT 0,
    settings        JSON,
    created_by_id   BIGINT,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at      DATETIME(3) NULL,
    CONSTRAINT fk_orgs_created_by FOREIGN KEY (created_by_id) REFERENCES users(id),
    CONSTRAINT org_name_slug CHECK (
        name REGEXP '^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$'
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Partial unique index approximation: MySQL doesn't support WHERE clauses.
-- This is a regular unique index on name (soft-deleted rows with same name will conflict).
CREATE UNIQUE INDEX organizations_name_active_idx ON organizations(name);

-- ============================================================
-- 5. organization_members
--    Column source: 000047 (base)
-- ============================================================
CREATE TABLE organization_members (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    user_id    BIGINT NOT NULL,
    role       VARCHAR(16) NOT NULL,
    joined_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at DATETIME(3) NULL,
    UNIQUE (org_id, user_id),
    CONSTRAINT fk_org_members_org FOREIGN KEY (org_id) REFERENCES organizations(id),
    CONSTRAINT fk_org_members_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_org_members_org_id ON organization_members(org_id);
CREATE INDEX idx_org_members_user_id ON organization_members(user_id);

-- ============================================================
-- 6. teams
--    Column source: 000009 (base), 000036 (deleted_at), 000047 (org_id + NOT NULL)
-- ============================================================
CREATE TABLE teams (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    name            VARCHAR(128) UNIQUE NOT NULL,
    display_name    VARCHAR(128) NOT NULL,
    description     TEXT,
    budget_limit    DECIMAL(12,2) NOT NULL DEFAULT 0,
    budget_period   VARCHAR(16) NOT NULL DEFAULT 'monthly',
    rpm_limit       INT NOT NULL DEFAULT 0,
    tpm_limit       INT NOT NULL DEFAULT 0,
    status          SMALLINT NOT NULL DEFAULT 1,
    org_id          BIGINT NOT NULL,
    created_by_id   BIGINT,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at      DATETIME(3) NULL,
    CONSTRAINT fk_teams_org FOREIGN KEY (org_id) REFERENCES organizations(id),
    CONSTRAINT fk_teams_created_by FOREIGN KEY (created_by_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_teams_created_by_id ON teams(created_by_id);
CREATE INDEX idx_teams_deleted_at ON teams(deleted_at);
CREATE INDEX idx_teams_org_id ON teams(org_id);

-- ============================================================
-- 7. team_members
--    Column source: 000009 (base), 000036 (deleted_at)
-- ============================================================
CREATE TABLE team_members (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    team_id    BIGINT NOT NULL,
    user_id    BIGINT NOT NULL,
    role       VARCHAR(16) NOT NULL DEFAULT 'member',
    joined_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at DATETIME(3) NULL,
    UNIQUE (team_id, user_id),
    CONSTRAINT fk_team_members_team FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE CASCADE,
    CONSTRAINT fk_team_members_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_team_members_user_id ON team_members(user_id);
CREATE INDEX idx_team_members_deleted_at ON team_members(deleted_at);

-- ============================================================
-- 8. role_permissions
--    Column source: 000012 (base)
-- ============================================================
CREATE TABLE role_permissions (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    role_id BIGINT NOT NULL,
    action  VARCHAR(64) NOT NULL,
    UNIQUE (role_id, action),
    CONSTRAINT fk_role_permissions_role FOREIGN KEY (role_id) REFERENCES roles(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ============================================================
-- 9. providers
--    Column source: 000001 (base), 000003 (CHECK), 000036 (deleted_at), 000047 (org_id)
-- ============================================================
CREATE TABLE providers (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    name          VARCHAR(64) UNIQUE NOT NULL,
    display_name  VARCHAR(128) NOT NULL,
    adapter_type  VARCHAR(32) NOT NULL,
    base_url      VARCHAR(512) NOT NULL,
    api_key       TEXT NOT NULL,
    extra_config  JSON,
    status        SMALLINT NOT NULL DEFAULT 1,
    org_id        BIGINT,
    created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at    DATETIME(3) NULL,
    CONSTRAINT fk_providers_org FOREIGN KEY (org_id) REFERENCES organizations(id),
    CONSTRAINT chk_adapter_type CHECK (
        adapter_type IN ('openai_compatible', 'anthropic', 'azure_openai', 'aws_bedrock', 'google_vertex', 'ollama')
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_providers_deleted_at ON providers(deleted_at);
CREATE INDEX idx_providers_org_id ON providers(org_id);

-- ============================================================
-- 10. provider_models
--    Column source: 000001 (base), 000004 (currency), 000013 (routing_strategy),
--                   000014 (CHECK update), 000036 (deleted_at), 000047 (org_id)
--    Note: original UNIQUE(provider_id,model_name,provider_model) replaced by
--    partial unique index (000045). MySQL has no partial indexes, so we use
--    a regular unique index.
-- ============================================================
CREATE TABLE provider_models (
    id               BIGINT AUTO_INCREMENT PRIMARY KEY,
    provider_id      BIGINT NOT NULL,
    model_name       VARCHAR(128) NOT NULL,
    provider_model   VARCHAR(128) NOT NULL,
    weight           INT NOT NULL DEFAULT 0,
    priority         INT NOT NULL DEFAULT 1,
    status           SMALLINT NOT NULL DEFAULT 1,
    max_context      INT,
    input_price      DECIMAL(10,6) NOT NULL DEFAULT 0,
    output_price     DECIMAL(10,6) NOT NULL DEFAULT 0,
    currency         VARCHAR(3) NOT NULL DEFAULT 'CNY',
    routing_strategy VARCHAR(32) NOT NULL DEFAULT 'weighted_random',
    extra_config     JSON,
    org_id           BIGINT,
    created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at       DATETIME(3) NULL,
    CONSTRAINT fk_provider_models_provider FOREIGN KEY (provider_id) REFERENCES providers(id) ON DELETE CASCADE,
    CONSTRAINT fk_provider_models_org FOREIGN KEY (org_id) REFERENCES organizations(id),
    CONSTRAINT chk_routing_strategy CHECK (
        routing_strategy IN ('weighted_random', 'round_robin', 'least_latency', 'least_cost', 'canary', 'least_busy')
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Partial unique index approximation (no WHERE clause in MySQL)
CREATE UNIQUE INDEX provider_models_active_unique
    ON provider_models(provider_id, model_name, provider_model);

CREATE INDEX idx_provider_models_deleted_at ON provider_models(deleted_at);
CREATE INDEX idx_provider_models_model_status ON provider_models(model_name, status);

-- ============================================================
-- 11. api_keys
--    Column source: 000001 (base), 000009 (created_by_id, team_id), 000011 (max_budget, budget_period),
--                   000036 (deleted_at), 000046 (email), 000047 (org_id), 000052 (max_calls, call_period)
-- ============================================================
CREATE TABLE api_keys (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    name                VARCHAR(128) NOT NULL,
    key_hash            VARCHAR(64) UNIQUE NOT NULL,
    key_prefix          VARCHAR(8) NOT NULL,
    status              SMALLINT NOT NULL DEFAULT 1,
    allowed_models      JSON,
    allowed_routes      JSON,
    tpm_limit           INT NOT NULL DEFAULT 0,
    rpm_limit           INT NOT NULL DEFAULT 0,
    max_budget          DECIMAL(12,4) NOT NULL DEFAULT 0,
    budget_period       VARCHAR(16) NOT NULL DEFAULT 'monthly',
    max_calls           INT NOT NULL DEFAULT 0,
    call_period         VARCHAR(16) NOT NULL DEFAULT 'daily',
    email               VARCHAR(255),
    created_by          VARCHAR(64) NOT NULL DEFAULT 'admin',
    created_by_id       BIGINT,
    team_id             BIGINT,
    org_id              BIGINT,
    expires_at          DATETIME(3) NULL,
    created_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    last_used_at        DATETIME(3) NULL,
    deleted_at          DATETIME(3) NULL,
    CONSTRAINT fk_api_keys_created_by FOREIGN KEY (created_by_id) REFERENCES users(id) ON DELETE SET NULL,
    CONSTRAINT fk_api_keys_team FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE SET NULL,
    CONSTRAINT fk_api_keys_org FOREIGN KEY (org_id) REFERENCES organizations(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_api_keys_created_by_id ON api_keys(created_by_id);
CREATE INDEX idx_api_keys_team_id ON api_keys(team_id);
CREATE INDEX idx_api_keys_deleted_at ON api_keys(deleted_at);
CREATE INDEX idx_api_keys_org_id ON api_keys(org_id);

-- ============================================================
-- 12. api_key_hashes
--    Column source: 000007 (base), 000008 (indexes), 000041 (hash_algo)
-- ============================================================
CREATE TABLE api_key_hashes (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    api_key_id  BIGINT NOT NULL,
    key_hash    VARCHAR(64) NOT NULL,
    key_prefix  VARCHAR(8) NOT NULL,
    hash_algo   VARCHAR(10) NOT NULL DEFAULT 'sha256',
    is_primary  TINYINT(1) NOT NULL DEFAULT 1,
    grace_until DATETIME(3) NULL,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    CONSTRAINT fk_api_key_hashes_key FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE UNIQUE INDEX idx_api_key_hashes_key_hash ON api_key_hashes(key_hash);
CREATE INDEX idx_api_key_hashes_api_key_id ON api_key_hashes(api_key_id);
-- Partial indexes removed (MySQL doesn't support WHERE clause):
--   idx_api_key_hashes_grace_until WHERE grace_until IS NOT NULL
--   idx_api_key_hashes_one_primary WHERE is_primary = 1

-- ============================================================
-- 13. usage_logs
--    Column source: 000001 (base), 000002 (user_message, model_response), 000005 (currency),
--                   000006 (cost precision DECIMAL(16,8)), 000009 (team_id),
--                   000015 (fallback_count), 000016 (retry_count),
--                   000019 (guardrail_triggered, guardrail_rule),
--                   000020 (cache_hit), 000034 (agent_type, security_events),
--                   000047 (org_id)
-- ============================================================
CREATE TABLE usage_logs (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    request_id        VARCHAR(64) NOT NULL,
    api_key_id        BIGINT,
    provider_id       BIGINT,
    route_type        VARCHAR(16) NOT NULL,
    model_requested   VARCHAR(128) NOT NULL,
    model_used        VARCHAR(128) NOT NULL,
    input_tokens      INT NOT NULL DEFAULT 0,
    output_tokens     INT NOT NULL DEFAULT 0,
    cost              DECIMAL(16,8) NOT NULL DEFAULT 0,
    latency_ms        INT NOT NULL DEFAULT 0,
    first_token_ms    INT,
    status_code       INT NOT NULL,
    error_type        VARCHAR(64),
    currency          VARCHAR(3) NOT NULL DEFAULT 'CNY',
    team_id           BIGINT,
    user_message      TEXT,
    model_response    TEXT,
    fallback_count    SMALLINT NOT NULL DEFAULT 0,
    retry_count       SMALLINT NOT NULL DEFAULT 0,
    guardrail_triggered TINYINT(1) DEFAULT 0,
    guardrail_rule    VARCHAR(255),
    cache_hit         TINYINT(1) NOT NULL DEFAULT 0,
    agent_type        VARCHAR(32),
    security_events   JSON DEFAULT ('[]'),
    org_id            BIGINT,
    reasoning_tokens  INT NOT NULL DEFAULT 0,
    cache_read_tokens INT NOT NULL DEFAULT 0,
    session_id        VARCHAR(255),
    created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    CONSTRAINT fk_usage_logs_key FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE SET NULL,
    CONSTRAINT fk_usage_logs_provider FOREIGN KEY (provider_id) REFERENCES providers(id) ON DELETE SET NULL,
    CONSTRAINT fk_usage_logs_team FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE SET NULL,
    CONSTRAINT fk_usage_logs_org FOREIGN KEY (org_id) REFERENCES organizations(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_usage_logs_api_key_id ON usage_logs(api_key_id);
CREATE INDEX idx_usage_logs_provider_id ON usage_logs(provider_id);
CREATE INDEX idx_usage_logs_created_at ON usage_logs(created_at);
CREATE INDEX idx_usage_logs_model_requested ON usage_logs(model_requested);
CREATE INDEX idx_usage_logs_currency ON usage_logs(currency);
CREATE INDEX idx_usage_logs_team_id ON usage_logs(team_id);
-- Partial indexes removed (MySQL doesn't support WHERE clause):
--   idx_usage_logs_fallback WHERE fallback_count > 0
--   idx_usage_logs_retry WHERE retry_count > 0
--   idx_usage_logs_guardrail WHERE guardrail_triggered = 1
--   idx_usage_logs_cache_hit WHERE cache_hit = 1
--   idx_usage_logs_agent_type WHERE agent_type IS NOT NULL
--   idx_usage_logs_error_type WHERE error_type IS NOT NULL
CREATE INDEX idx_usage_logs_org_id ON usage_logs(org_id);
CREATE INDEX idx_usage_logs_session_id ON usage_logs(session_id);
CREATE INDEX idx_usage_logs_status_code ON usage_logs(status_code);

-- ============================================================
-- 14. guardrail_rules
--    Column source: 000018 (base), 000047 (org_id)
-- ============================================================
CREATE TABLE guardrail_rules (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    name         VARCHAR(255) NOT NULL,
    type         VARCHAR(50) NOT NULL,
    direction    VARCHAR(10) NOT NULL,
    enabled      TINYINT(1) DEFAULT 1,
    config       JSON NOT NULL,
    severity     VARCHAR(20) DEFAULT 'medium',
    action       VARCHAR(20) DEFAULT 'block',
    model_filter TEXT,
    org_id       BIGINT,
    created_at   DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_guardrail_rules_type ON guardrail_rules(type);
-- Partial index removed: idx_guardrail_rules_enabled WHERE enabled = 1

-- ============================================================
-- 15. guardrail_alert_rules
--    Column source: 000026 (base), 000028 (TIMESTAMPTZ fix), 000047 (org_id)
-- ============================================================
CREATE TABLE guardrail_alert_rules (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    rule_id           BIGINT NOT NULL,
    team_id           BIGINT,
    channels          JSON NOT NULL DEFAULT ('[]'),
    cooldown_minutes  INT NOT NULL DEFAULT 5,
    enabled           TINYINT(1) NOT NULL DEFAULT 1,
    last_triggered_at DATETIME(3) NULL,
    org_id            BIGINT,
    created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE (rule_id),
    CONSTRAINT fk_alert_rules_rule FOREIGN KEY (rule_id) REFERENCES guardrail_rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_alert_rules_team_id ON guardrail_alert_rules(team_id);

-- ============================================================
-- 16. guardrail_alert_logs
--    Column source: 000026 (base), 000028 (TIMESTAMPTZ fix), 000029 (indexes),
--                   000034 (agent_type), 000047 (org_id)
-- ============================================================
CREATE TABLE guardrail_alert_logs (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    rule_id         BIGINT NOT NULL,
    alert_rule_id   BIGINT,
    rule_name       VARCHAR(255),
    engine_type     VARCHAR(50),
    severity        VARCHAR(20),
    action          VARCHAR(20),
    direction       VARCHAR(10),
    reason          VARCHAR(1000),
    model           VARCHAR(255),
    content_preview VARCHAR(500),
    api_key_id      BIGINT,
    team_id         BIGINT,
    channels        VARCHAR(500),
    status          VARCHAR(20),
    agent_type      VARCHAR(32),
    org_id          BIGINT,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_alert_logs_rule_id ON guardrail_alert_logs(rule_id);
CREATE INDEX idx_alert_logs_created_at ON guardrail_alert_logs(created_at);
CREATE INDEX idx_alert_logs_team_id ON guardrail_alert_logs(team_id);
CREATE INDEX idx_alert_logs_severity ON guardrail_alert_logs(severity);
CREATE INDEX idx_alert_logs_action ON guardrail_alert_logs(action);

-- ============================================================
-- 17. budget_alerts
--    Column source: 000011 (base), 000036 (deleted_at), 000047 (org_id, updated CHECK)
-- ============================================================
CREATE TABLE budget_alerts (
    id               BIGINT AUTO_INCREMENT PRIMARY KEY,
    team_id          BIGINT,
    key_id           BIGINT,
    org_id           BIGINT,
    threshold_pct    SMALLINT NOT NULL,
    webhook_url      VARCHAR(512) NOT NULL,
    last_triggered_at DATETIME(3) NULL,
    created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at       DATETIME(3) NULL,
    CONSTRAINT fk_budget_alerts_team FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE CASCADE,
    CONSTRAINT fk_budget_alerts_key FOREIGN KEY (key_id) REFERENCES api_keys(id) ON DELETE CASCADE,
    CONSTRAINT chk_alert_target CHECK (
        (org_id IS NOT NULL) + (team_id IS NOT NULL) + (key_id IS NOT NULL) = 1
    ),
    CONSTRAINT chk_alert_threshold CHECK (threshold_pct > 0 AND threshold_pct <= 100)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_budget_alerts_team ON budget_alerts(team_id);
CREATE INDEX idx_budget_alerts_key ON budget_alerts(key_id);
CREATE INDEX idx_budget_alerts_deleted_at ON budget_alerts(deleted_at);

-- ============================================================
-- 18. budget_snapshots
--    Column source: 000011 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE budget_snapshots (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    target_type VARCHAR(16) NOT NULL,
    target_id   BIGINT NOT NULL,
    period_key  VARCHAR(16) NOT NULL,
    spent       DECIMAL(16,8) NOT NULL DEFAULT 0,
    budget      DECIMAL(12,4) NOT NULL DEFAULT 0,
    currency    VARCHAR(3) NOT NULL DEFAULT 'CNY',
    org_id      BIGINT,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE (target_type, target_id, period_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_budget_snapshots_org_id ON budget_snapshots(org_id);

-- ============================================================
-- 19. budget_recommendations
--    Column source: 000030 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE budget_recommendations (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    target_type         VARCHAR(16) NOT NULL,
    target_id           BIGINT NOT NULL,
    period              VARCHAR(16) NOT NULL DEFAULT 'monthly',
    recommended_budget  DECIMAL(12,2) NOT NULL,
    current_budget      DECIMAL(12,2) NOT NULL DEFAULT 0,
    avg_period_spend    DECIMAL(12,2) NOT NULL DEFAULT 0,
    growth_rate         DECIMAL(8,4) NOT NULL DEFAULT 0,
    confidence          DECIMAL(5,2) NOT NULL DEFAULT 0,
    reasoning           TEXT NOT NULL,
    currency            VARCHAR(3) NOT NULL DEFAULT 'CNY',
    org_id              BIGINT,
    created_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_budget_recs_target ON budget_recommendations(target_type, target_id);
CREATE INDEX idx_budget_recs_created ON budget_recommendations(created_at DESC);
CREATE INDEX idx_budget_recommendations_org_id ON budget_recommendations(org_id);

-- ============================================================
-- 20. budget_requests
--    Column source: 000030 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE budget_requests (
    id                 BIGINT AUTO_INCREMENT PRIMARY KEY,
    target_type        VARCHAR(16) NOT NULL,
    target_id          BIGINT NOT NULL,
    period             VARCHAR(16) NOT NULL DEFAULT 'monthly',
    current_budget     DECIMAL(12,2) NOT NULL DEFAULT 0,
    requested_budget   DECIMAL(12,2) NOT NULL,
    reason             TEXT NOT NULL,
    recommendation_id  BIGINT,
    status             VARCHAR(16) NOT NULL DEFAULT 'pending',
    created_by         BIGINT NOT NULL,
    reviewed_by        BIGINT,
    review_comment     TEXT NOT NULL,
    reviewed_at        DATETIME(3) NULL,
    org_id             BIGINT,
    created_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_budget_reqs_target ON budget_requests(target_type, target_id);
CREATE INDEX idx_budget_reqs_status ON budget_requests(status);
CREATE INDEX idx_budget_reqs_creator ON budget_requests(created_by);
CREATE INDEX idx_budget_reqs_reviewer ON budget_requests(reviewed_by);
CREATE INDEX idx_budget_reqs_created ON budget_requests(created_at DESC);
CREATE INDEX idx_budget_requests_org_id ON budget_requests(org_id);

-- ============================================================
-- 21. audit_logs
--    Column source: 000023 (base), 000024 (composite indexes), 000047 (org_id)
-- ============================================================
CREATE TABLE audit_logs (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id        BIGINT NOT NULL,
    username       VARCHAR(64) NOT NULL,
    action         VARCHAR(64) NOT NULL,
    resource_type  VARCHAR(32) NOT NULL,
    resource_id    VARCHAR(64) NOT NULL,
    resource_name  VARCHAR(128),
    detail         JSON,
    ip_address     VARCHAR(45),
    user_agent     VARCHAR(512),
    status         VARCHAR(16) NOT NULL DEFAULT 'success',
    org_id         BIGINT,
    created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_audit_logs_user_id ON audit_logs(user_id);
CREATE INDEX idx_audit_logs_action ON audit_logs(action);
CREATE INDEX idx_audit_logs_resource ON audit_logs(resource_type, resource_id);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at);
CREATE INDEX idx_audit_logs_action_created ON audit_logs(action, created_at DESC);
CREATE INDEX idx_audit_logs_resource_type_created ON audit_logs(resource_type, created_at DESC);
CREATE INDEX idx_audit_logs_org_id ON audit_logs(org_id);

-- ============================================================
-- 22. insights
--    Column source: 000031 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE insights (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    period       VARCHAR(16) NOT NULL DEFAULT 'monthly',
    period_key   VARCHAR(16) NOT NULL,
    scope        VARCHAR(16) NOT NULL DEFAULT 'global',
    scope_id     BIGINT NOT NULL DEFAULT 0,
    insight_type VARCHAR(32) NOT NULL DEFAULT 'summary',
    title        VARCHAR(256) NOT NULL,
    content      TEXT NOT NULL,
    model_used   VARCHAR(128) NOT NULL,
    tokens_used  INT NOT NULL DEFAULT 0,
    org_id       BIGINT,
    created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_insights_period ON insights(period_key DESC);
CREATE INDEX idx_insights_scope ON insights(scope, scope_id);
CREATE INDEX idx_insights_type ON insights(insight_type);
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);
CREATE INDEX idx_insights_org_id ON insights(org_id);

-- ============================================================
-- 23. optimization_actions
--    Column source: 000032 (base), 000047 (org_id), 000048 (re-add)
-- ============================================================
CREATE TABLE optimization_actions (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    action_type     VARCHAR(32) NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    priority        VARCHAR(16) NOT NULL DEFAULT 'medium',
    status          VARCHAR(16) NOT NULL DEFAULT 'pending',
    payload         JSON NOT NULL DEFAULT ('{}'),
    saving_estimate DECIMAL(12,2) DEFAULT 0,
    org_id          BIGINT,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    applied_at      DATETIME(3) NULL,
    applied_by      BIGINT,
    dismissed_at    DATETIME(3) NULL,
    dismissed_by    BIGINT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_opt_actions_status ON optimization_actions(status);
CREATE INDEX idx_opt_actions_type ON optimization_actions(action_type);
CREATE INDEX idx_opt_actions_priority ON optimization_actions(priority);
CREATE INDEX idx_opt_actions_created ON optimization_actions(created_at DESC);
CREATE INDEX idx_optimization_actions_org_id ON optimization_actions(org_id);

-- ============================================================
-- 24. agent_fingerprints
--    Column source: 000035 (base), 000047 (org_id)
-- ============================================================
CREATE TABLE agent_fingerprints (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    name            VARCHAR(64) NOT NULL,
    source_type     VARCHAR(16) NOT NULL DEFAULT 'header',
    source_field    VARCHAR(128) NOT NULL,
    pattern         TEXT NOT NULL,
    risk_level      VARCHAR(16) NOT NULL DEFAULT 'medium',
    origin          VARCHAR(16) NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'active',
    hit_count       BIGINT NOT NULL DEFAULT 0,
    last_hit_at     DATETIME(3) NULL,
    discovered_from JSON,
    org_id          BIGINT,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_agent_fingerprints_status ON agent_fingerprints(status);
CREATE INDEX idx_agent_fingerprints_name ON agent_fingerprints(name);
CREATE INDEX idx_agent_fingerprints_origin ON agent_fingerprints(origin);
CREATE INDEX idx_agent_fingerprints_status_type ON agent_fingerprints(status, source_type, source_field);

-- Dedup index: PG uses md5(pattern) to bound index size; MySQL uses full TEXT pattern.
CREATE UNIQUE INDEX idx_agent_fingerprints_dedup
    ON agent_fingerprints(source_type, source_field, pattern(255));

-- ============================================================
-- 25. mcp_servers
--    Column source: 000038 (base), 000048 (org_id)
-- ============================================================
CREATE TABLE mcp_servers (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    name              VARCHAR(64) NOT NULL UNIQUE,
    display_name      VARCHAR(128),
    description       VARCHAR(512),
    transport_type    VARCHAR(16) NOT NULL,
    url               VARCHAR(512),
    stdio_config      JSON,
    auth_type         VARCHAR(32) DEFAULT 'none',
    auth_config       JSON,
    custom_headers    JSON DEFAULT ('{}'),
    status            SMALLINT NOT NULL DEFAULT 1,
    health_status     SMALLINT NOT NULL DEFAULT 0,
    last_health_check DATETIME(3) NULL,
    tool_count        INT DEFAULT 0,
    enabled           TINYINT(1) DEFAULT 1,
    tier_required     VARCHAR(16) DEFAULT 'community',
    created_by        BIGINT,
    org_id            BIGINT,
    created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at        DATETIME(3) NULL,
    CONSTRAINT chk_transport_type CHECK (
        transport_type IN ('http', 'sse', 'stdio')
    ),
    CONSTRAINT chk_auth_type CHECK (
        auth_type IN ('none', 'bearer', 'basic', 'oauth2', 'sigv4')
    ),
    CONSTRAINT chk_transport_config CHECK (
        (transport_type = 'stdio' AND stdio_config IS NOT NULL) OR
        (transport_type IN ('http', 'sse') AND url IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_mcp_servers_status ON mcp_servers(status);
CREATE INDEX idx_mcp_servers_enabled ON mcp_servers(enabled);
CREATE INDEX idx_mcp_servers_deleted ON mcp_servers(deleted_at);
CREATE INDEX idx_mcp_servers_org_id ON mcp_servers(org_id);

-- ============================================================
-- 26. mcp_server_permissions
--    Column source: 000038 (base)
-- ============================================================
CREATE TABLE mcp_server_permissions (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    server_id      BIGINT NOT NULL,
    principal_type VARCHAR(16) NOT NULL,
    principal_id   BIGINT NOT NULL,
    allow_tools    JSON DEFAULT ('["*"]'),
    deny_tools     JSON DEFAULT ('[]'),
    created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE (server_id, principal_type, principal_id),
    CONSTRAINT fk_mcp_perm_server FOREIGN KEY (server_id) REFERENCES mcp_servers(id) ON DELETE CASCADE,
    CONSTRAINT chk_principal_type CHECK (
        principal_type IN ('key', 'team', 'role')
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_mcp_perm_principal ON mcp_server_permissions(principal_type, principal_id);

-- ============================================================
-- 27. mcp_tool_call_logs — partitioned by RANGE COLUMNS(created_at)
--    Column source: 000038 (base, partition parent), 000039 (partitions), 000048 (org_id)
--    MySQL range partitioning with monthly partitions for 2026 and a catch-all.
-- ============================================================
CREATE TABLE mcp_tool_call_logs (
    id          BIGINT AUTO_INCREMENT,
    request_id  VARCHAR(36),
    server_id   BIGINT,
    server_name VARCHAR(64),
    tool_name   VARCHAR(128),
    method      VARCHAR(32),
    input_size  INT DEFAULT 0,
    output_size INT DEFAULT 0,
    duration    INT DEFAULT 0,
    status      SMALLINT NOT NULL,
    error_code  INT,
    error_msg   VARCHAR(512),
    api_key_id  BIGINT,
    user_id     BIGINT,
    team_id     BIGINT,
    blocked_by  VARCHAR(64),
    org_id      BIGINT,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id, created_at),
    INDEX idx_mcp_logs_server (server_id),
    INDEX idx_mcp_logs_key (api_key_id),
    INDEX idx_mcp_logs_team (team_id),
    INDEX idx_mcp_logs_time (created_at),
    INDEX idx_mcp_logs_org_id (org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  PARTITION BY RANGE COLUMNS(created_at) (
    PARTITION p_2026_01 VALUES LESS THAN ('2026-02-01 00:00:00.000'),
    PARTITION p_2026_02 VALUES LESS THAN ('2026-03-01 00:00:00.000'),
    PARTITION p_2026_03 VALUES LESS THAN ('2026-04-01 00:00:00.000'),
    PARTITION p_2026_04 VALUES LESS THAN ('2026-05-01 00:00:00.000'),
    PARTITION p_2026_05 VALUES LESS THAN ('2026-06-01 00:00:00.000'),
    PARTITION p_2026_06 VALUES LESS THAN ('2026-07-01 00:00:00.000'),
    PARTITION p_2026_07 VALUES LESS THAN ('2026-08-01 00:00:00.000'),
    PARTITION p_2026_08 VALUES LESS THAN ('2026-09-01 00:00:00.000'),
    PARTITION p_2026_09 VALUES LESS THAN ('2026-10-01 00:00:00.000'),
    PARTITION p_2026_10 VALUES LESS THAN ('2026-11-01 00:00:00.000'),
    PARTITION p_2026_11 VALUES LESS THAN ('2026-12-01 00:00:00.000'),
    PARTITION p_2026_12 VALUES LESS THAN ('2027-01-01 00:00:00.000'),
    PARTITION p_future   VALUES LESS THAN ('2030-01-01 00:00:00.000')
);

-- ============================================================
-- 28. mcp_tool_call_logs_archive
--    Archive table for aged-out MCP tool call logs.
-- ============================================================
CREATE TABLE mcp_tool_call_logs_archive (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    request_id  VARCHAR(36),
    server_id   BIGINT,
    server_name VARCHAR(64),
    tool_name   VARCHAR(128),
    method      VARCHAR(32),
    input_size  INT DEFAULT 0,
    output_size INT DEFAULT 0,
    duration    INT DEFAULT 0,
    status      SMALLINT NOT NULL,
    error_code  INT,
    error_msg   VARCHAR(512),
    api_key_id  BIGINT,
    user_id     BIGINT,
    team_id     BIGINT,
    blocked_by  VARCHAR(64),
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ============================================================
-- 29. datalens_hourly_metrics — DataLens hourly pre-aggregation
--    Column source: 000056 (base), 000058 (agg_type widen)
--    Note: PG COALESCE unique index replaced with regular indexes;
--    UPSERT handled via ON DUPLICATE KEY UPDATE in Go code.
-- ============================================================
CREATE TABLE datalens_hourly_metrics (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id          BIGINT NOT NULL,
    agg_level       VARCHAR(16) NOT NULL,
    team_id         BIGINT,
    api_key_id      BIGINT,
    provider_id     BIGINT,
    model_name      VARCHAR(128),
    route_type      VARCHAR(16),
    status_group    INT NOT NULL DEFAULT 200,
    hour_bucket     DATETIME(3) NOT NULL,
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

    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- COALESCE unique index omitted (MySQL lacks expression indexes).
-- UPSERT logic handled in Go code via query-then-insert or ON DUPLICATE KEY UPDATE.
CREATE INDEX idx_dhm_org_hour      ON datalens_hourly_metrics (org_id, hour_bucket);
CREATE INDEX idx_dhm_level_hour    ON datalens_hourly_metrics (org_id, agg_level, hour_bucket);
CREATE INDEX idx_dhm_model_hour    ON datalens_hourly_metrics (org_id, model_name, hour_bucket);
CREATE INDEX idx_dhm_team_hour     ON datalens_hourly_metrics (org_id, team_id, hour_bucket);
CREATE INDEX idx_dhm_key_hour      ON datalens_hourly_metrics (org_id, api_key_id, hour_bucket);
CREATE INDEX idx_dhm_provider_hour ON datalens_hourly_metrics (org_id, provider_id, hour_bucket);

-- ============================================================
-- 30. datalens_daily_metrics — DataLens daily pre-aggregation
--    Column source: 000056 (base)
--    Note: PG COALESCE unique index replaced with regular indexes.
-- ============================================================
CREATE TABLE datalens_daily_metrics (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id          BIGINT NOT NULL,
    agg_level       VARCHAR(16) NOT NULL,
    team_id         BIGINT,
    api_key_id      BIGINT,
    provider_id     BIGINT,
    model_name      VARCHAR(128),
    route_type      VARCHAR(16),
    status_group    INT NOT NULL DEFAULT 200,
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

    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- COALESCE unique index omitted (MySQL lacks expression indexes).
CREATE INDEX idx_ddm_org_day ON datalens_daily_metrics (org_id, day_bucket);

-- ============================================================
-- 31. datalens_agg_status — DataLens aggregation health status
--    Column source: 000056 (base), 000058 (agg_type VARCHAR(32))
-- ============================================================
CREATE TABLE datalens_agg_status (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    agg_level       VARCHAR(16) NOT NULL,
    agg_type        VARCHAR(32) NOT NULL,
    last_success_at DATETIME(3) NOT NULL,
    last_duration_ms INT NOT NULL DEFAULT 0,
    rows_affected   INT NOT NULL DEFAULT 0,
    error_message   TEXT,
    updated_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE UNIQUE INDEX idx_das_level_type ON datalens_agg_status (agg_level, agg_type);

-- ============================================================
-- 32. datalens_reports — DataLens saved report configurations
--    Column source: 000056 (base)
-- ============================================================
CREATE TABLE datalens_reports (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id      BIGINT NOT NULL,
    user_id     BIGINT NOT NULL,
    name        VARCHAR(128) NOT NULL,
    description TEXT,
    type        VARCHAR(16) NOT NULL DEFAULT 'custom',
    template_id VARCHAR(64),
    scope       VARCHAR(16) NOT NULL DEFAULT 'private',
    config      JSON NOT NULL,
    is_pinned   TINYINT(1) NOT NULL DEFAULT 0,
    version     INT NOT NULL DEFAULT 1,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at  DATETIME(3) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Partial indexes removed (MySQL doesn't support WHERE clause):
--   idx_dr_org_user ON (org_id, user_id) WHERE deleted_at IS NULL
--   idx_dr_template ON (org_id, template_id) WHERE deleted_at IS NULL
--   idx_dr_deleted  ON (deleted_at) WHERE deleted_at IS NOT NULL
CREATE INDEX idx_dr_org_user ON datalens_reports (org_id, user_id);
CREATE INDEX idx_dr_template ON datalens_reports (org_id, template_id);
CREATE INDEX idx_dr_deleted  ON datalens_reports (deleted_at);

-- ============================================================
-- 33. datalens_schedules — DataLens automated report schedules (Enterprise)
--    Column source: 000056 (base)
-- ============================================================
CREATE TABLE datalens_schedules (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id      BIGINT NOT NULL,
    user_id     BIGINT NOT NULL,
    team_id     BIGINT,
    name        VARCHAR(128) NOT NULL,
    report_id   BIGINT NOT NULL,
    cron        VARCHAR(64) NOT NULL,
    timezone    VARCHAR(32) NOT NULL DEFAULT 'Asia/Shanghai',
    channels    JSON NOT NULL,
    enabled     TINYINT(1) NOT NULL DEFAULT 1,
    last_run_at DATETIME(3) NULL,
    next_run_at DATETIME(3) NULL,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at  DATETIME(3) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Partial index removed: idx_ds_org ON (org_id, enabled, next_run_at) WHERE deleted_at IS NULL
CREATE INDEX idx_ds_org ON datalens_schedules (org_id, enabled, next_run_at);

-- ============================================================
-- 34. datalens_partition_marker — DataLens partition migration tracking
--    Column source: 000057 (base)
-- ============================================================
CREATE TABLE datalens_partition_marker (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    partitioned  TINYINT(1) NOT NULL DEFAULT 0,
    migrated_at  DATETIME(3) NULL,
    note         TEXT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO datalens_partition_marker (partitioned, note) VALUES (0, 'Partition migration pending. Run: crosslink migrate-partition');

-- Error classification rules (global, platform-level config for failover precision)
CREATE TABLE error_classification_rules (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    match_field    VARCHAR(16) NOT NULL,
    pattern        VARCHAR(128) NOT NULL,
    classification VARCHAR(16) NOT NULL DEFAULT 'quota',
    provider_type  VARCHAR(32) NULL,
    scope          VARCHAR(16) NOT NULL DEFAULT 'account',
    priority       INT NOT NULL DEFAULT 100,
    enabled        TINYINT(1) NOT NULL DEFAULT 1,
    created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE INDEX idx_error_rules_enabled ON error_classification_rules(enabled);

INSERT INTO error_classification_rules (match_field, pattern, provider_type, scope) VALUES
    ('code', 'insufficient_quota',         'openai_compatible', 'account'),
    ('code', 'quota_exceeded',             'openai_compatible', 'account'),
    ('code', 'billing_hard_limit_reached', 'openai_compatible', 'account'),
    ('type', 'model_deprecated',           'openai_compatible', 'model'),
    ('type', 'billing_disabled',           'anthropic',         'account'),
    ('status', '402',                      NULL,                'account');

-- Capability routing: user-defined capability groups mapping model names to capabilities.
CREATE TABLE IF NOT EXISTS capabilities (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id      BIGINT       NOT NULL DEFAULT 0,
    name        VARCHAR(64)  NOT NULL,
    modality    VARCHAR(16)  NOT NULL DEFAULT 'text',
    status      SMALLINT     NOT NULL DEFAULT 1,
    created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uq_capabilities_org_name (org_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS capability_members (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    capability_id  BIGINT       NOT NULL,
    model_name     VARCHAR(128) NOT NULL,
    quality_score  INT          NOT NULL DEFAULT 0,
    status         SMALLINT     NOT NULL DEFAULT 1,
    created_at     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    CONSTRAINT fk_cm_capability FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE,
    UNIQUE KEY uq_cm_cap_model (capability_id, model_name),
    KEY idx_cm_capability (capability_id),
    KEY idx_cm_quality (capability_id, quality_score)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
