CREATE TABLE api_keys (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    key_hash VARCHAR(64) UNIQUE NOT NULL,
    key_prefix VARCHAR(8) NOT NULL,
    status SMALLINT NOT NULL DEFAULT 1,
    allowed_models JSONB,
    allowed_routes JSONB,
    tpm_limit INT NOT NULL DEFAULT 0,
    rpm_limit INT NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    created_by VARCHAR(64) NOT NULL DEFAULT 'admin'
);

CREATE TABLE providers (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(64) UNIQUE NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    adapter_type VARCHAR(32) NOT NULL,
    base_url VARCHAR(512) NOT NULL,
    api_key TEXT NOT NULL,
    extra_config JSONB,
    status SMALLINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE provider_models (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_name VARCHAR(128) NOT NULL,
    provider_model VARCHAR(128) NOT NULL,
    weight INT NOT NULL DEFAULT 0,
    priority INT NOT NULL DEFAULT 1,
    status SMALLINT NOT NULL DEFAULT 1,
    max_context INT,
    input_price DECIMAL(10,6) NOT NULL DEFAULT 0,
    output_price DECIMAL(10,6) NOT NULL DEFAULT 0,
    extra_config JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider_id, model_name, provider_model)
);

CREATE TABLE usage_logs (
    id BIGSERIAL PRIMARY KEY,
    request_id VARCHAR(64) NOT NULL,
    api_key_id BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
    provider_id BIGINT REFERENCES providers(id) ON DELETE SET NULL,
    route_type VARCHAR(16) NOT NULL,
    model_requested VARCHAR(128) NOT NULL,
    model_used VARCHAR(128) NOT NULL,
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cost DECIMAL(12,6) NOT NULL DEFAULT 0,
    latency_ms INT NOT NULL DEFAULT 0,
    first_token_ms INT,
    status_code INT NOT NULL,
    error_type VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_usage_logs_api_key_id ON usage_logs(api_key_id);
CREATE INDEX idx_usage_logs_provider_id ON usage_logs(provider_id);
CREATE INDEX idx_usage_logs_created_at ON usage_logs(created_at);
CREATE INDEX idx_usage_logs_model_requested ON usage_logs(model_requested);

CREATE TABLE system_settings (
    key VARCHAR(128) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
