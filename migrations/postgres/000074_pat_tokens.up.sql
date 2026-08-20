-- Scoped personal access tokens (PAT): per-user tokens with fine-grained scopes
-- (budget:read / health:read / pat:manage) for CI and personal API access.
CREATE TABLE pat_tokens (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    name VARCHAR(128) NOT NULL,
    token_hash VARCHAR(64) NOT NULL UNIQUE,
    scopes JSONB NOT NULL,
    status SMALLINT NOT NULL DEFAULT 1,
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX idx_pat_tokens_user_id ON pat_tokens(user_id);

-- Composite index speeding up per-key usage queries over time-ordered logs.
CREATE INDEX idx_usage_logs_api_key_created_at ON usage_logs(api_key_id, created_at);
