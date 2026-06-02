CREATE INDEX idx_api_key_hashes_grace_until ON api_key_hashes(grace_until) WHERE grace_until IS NOT NULL;
CREATE UNIQUE INDEX idx_api_key_hashes_one_primary ON api_key_hashes(api_key_id) WHERE is_primary = true;
