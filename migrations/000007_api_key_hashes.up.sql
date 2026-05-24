CREATE TABLE api_key_hashes (
    id BIGSERIAL PRIMARY KEY,
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    key_hash VARCHAR(64) NOT NULL,
    key_prefix VARCHAR(8) NOT NULL,
    is_primary BOOLEAN NOT NULL DEFAULT true,
    grace_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_api_key_hashes_key_hash ON api_key_hashes(key_hash);
CREATE INDEX idx_api_key_hashes_api_key_id ON api_key_hashes(api_key_id);

-- Migrate existing key_hash/key_prefix from api_keys into api_key_hashes
INSERT INTO api_key_hashes (api_key_id, key_hash, key_prefix, is_primary)
SELECT id, key_hash, key_prefix, true FROM api_keys WHERE key_hash IS NOT NULL;
