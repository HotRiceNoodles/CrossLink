-- Schema parity catch-up: mysql short chain (consolidated baseline) is missing
-- objects that only the postgres chain received (migrations 000054–000078).
-- Verified against a fresh postgres-87 deployment via information_schema diff:
--
--   missing tables : mock_fixtures / sso_providers / pat_tokens /
--                    prompt_templates / provider_guard_alert_rules
--   missing columns: users.sso_provider_id, users.sso_id
--                    api_keys.price_multiplier / key_source / source_vendor
--                    datalens_hourly_metrics.13×ctx_* / datalens_daily_metrics.13×ctx_*
--   missing seeds  : the 3 system roles (admin/member/viewer) — their INSERTs
--                    were lost when old 000012_configurable_rbac was merged
--                    into the consolidated 000001, so ensureAdminUser skipped
--                    and nobody could log in on a fresh DB. Permissions are
--                    granted at boot by syncAdminPermissions / syncSystemRole-
--                    Permissions; only the role rows belong here.
--
-- postgres-only artifacts (mcp_tool_call_logs_YYYY_MM partitions, _default)
-- are intentionally NOT replicated: mysql uses mcp_tool_call_logs_archive.
-- Idempotent guards (IF NOT EXISTS / WHERE NOT EXISTS) keep the upgrade path
-- safe for existing 11-version deployments.

-- ==== missing tables ====

-- VCR playback: recorded (request → response) pairs (pg 000069)
CREATE TABLE IF NOT EXISTS mock_fixtures (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    provider_name VARCHAR(64)  NOT NULL,
    model         VARCHAR(128) NOT NULL,
    request_hash  VARCHAR(64)  NOT NULL,
    response_body JSON,
    stream_chunks JSON,
    is_stream     TINYINT(1)   NOT NULL DEFAULT 0,
    created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE INDEX idx_mock_fixtures_lookup ON mock_fixtures (request_hash, model);
CREATE INDEX idx_mock_fixtures_provider ON mock_fixtures (provider_name);

-- SSO identity providers + users SSO columns (pg 000054)
CREATE TABLE IF NOT EXISTS sso_providers (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    name              VARCHAR(64)  NOT NULL,
    type              VARCHAR(16)  NOT NULL DEFAULT 'oidc',
    issuer            VARCHAR(512) NOT NULL,
    client_id         VARCHAR(256) NOT NULL,
    client_secret     TEXT         NOT NULL,
    scopes            VARCHAR(256) DEFAULT 'openid profile email',
    default_org_id    BIGINT,
    default_role_id   BIGINT,
    auto_create       TINYINT(1)   NOT NULL DEFAULT 1,
    redirect_base_url VARCHAR(512),
    auth_url          VARCHAR(512),
    token_url         VARCHAR(512),
    userinfo_url      VARCHAR(512),
    enabled           TINYINT(1)   NOT NULL DEFAULT 1,
    created_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
ALTER TABLE users ADD COLUMN sso_provider_id BIGINT;
ALTER TABLE users ADD COLUMN sso_id VARCHAR(256) DEFAULT '';
-- pg uses a partial unique index; mysql has no partial indexes — full unique
-- on the IdP identity pair (rows with NULL provider are exempt by MySQL
-- NULL-never-equal semantics).
CREATE UNIQUE INDEX idx_users_sso ON users (sso_provider_id, sso_id);

-- Scoped personal access tokens (pg 000074)
CREATE TABLE IF NOT EXISTS pat_tokens (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT        NOT NULL,
    name        VARCHAR(128)  NOT NULL,
    token_hash  VARCHAR(64)   NOT NULL UNIQUE,
    scopes      JSON          NOT NULL,
    status      SMALLINT      NOT NULL DEFAULT 1,
    expires_at  DATETIME(3)   NOT NULL,
    last_used_at DATETIME(3),
    created_at  DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    revoked_at  DATETIME(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE INDEX idx_pat_tokens_user_id ON pat_tokens (user_id);

-- Prompt templates (pg 000068)
CREATE TABLE IF NOT EXISTS prompt_templates (
    id               BIGINT AUTO_INCREMENT PRIMARY KEY,
    name             VARCHAR(64)  NOT NULL,
    description      VARCHAR(512),
    system_prompt    TEXT         NOT NULL,
    variables_schema JSON,
    few_shot         JSON,
    tool_defs        JSON,
    target_format    VARCHAR(16)  NOT NULL DEFAULT 'auto',
    status           SMALLINT     NOT NULL DEFAULT 1,
    version          INT          NOT NULL DEFAULT 1,
    org_id           BIGINT,
    created_at       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at       DATETIME(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
-- pg's partial unique (WHERE deleted_at IS NULL) has no mysql equivalent;
-- plain index only — application layer enforces the soft-delete-aware rule.
CREATE INDEX idx_prompt_templates_name ON prompt_templates (name);
CREATE INDEX idx_prompt_templates_deleted_at ON prompt_templates (deleted_at);

-- Provider guard alert rules (pg 000066)
CREATE TABLE IF NOT EXISTS provider_guard_alert_rules (
    id               BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id           BIGINT,
    provider_name    VARCHAR(64)  NOT NULL DEFAULT '',
    model_name       VARCHAR(128) NOT NULL DEFAULT '',
    limit_type       VARCHAR(16)  NOT NULL DEFAULT '',
    channels         JSON         NOT NULL,
    cooldown_seconds INT          NOT NULL DEFAULT 300,
    enabled          TINYINT(1)   NOT NULL DEFAULT 1,
    last_triggered_at DATETIME(3),
    created_at       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE INDEX idx_pgar_org_enabled ON provider_guard_alert_rules (org_id, enabled);

-- ==== missing columns ====

-- per-key pricing (pg 000070) + key source attribution (pg 000078)
ALTER TABLE api_keys ADD COLUMN price_multiplier DECIMAL(6,4) NOT NULL DEFAULT 1.0000;
ALTER TABLE api_keys ADD COLUMN key_source VARCHAR(16) NOT NULL DEFAULT 'native';
ALTER TABLE api_keys ADD COLUMN source_vendor VARCHAR(32) NOT NULL DEFAULT '';

-- DataLens context-window metrics (pg 000073), hourly + daily
ALTER TABLE datalens_hourly_metrics
    ADD COLUMN ctx_system_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_history_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_question_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_output_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_total_window BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_analyzed_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_overflow_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_window_unknown_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_lt50 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_50_80 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_80_95 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_gt95 INT NOT NULL DEFAULT 0;
ALTER TABLE datalens_daily_metrics
    ADD COLUMN ctx_system_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_history_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_question_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_output_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_total_window BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_analyzed_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_overflow_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_window_unknown_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_lt50 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_50_80 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_80_95 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_gt95 INT NOT NULL DEFAULT 0;

-- ==== missing seeds: system roles ====
INSERT INTO roles (name, display_name, is_system, created_at)
SELECT 'admin', 'Administrator', 1, NOW(3) FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = 'admin');
INSERT INTO roles (name, display_name, is_system, created_at)
SELECT 'member', 'Member', 1, NOW(3) FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = 'member');
INSERT INTO roles (name, display_name, is_system, created_at)
SELECT 'viewer', 'Viewer', 1, NOW(3) FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = 'viewer');
