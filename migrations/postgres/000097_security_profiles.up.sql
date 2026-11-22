CREATE TABLE security_profiles (
    id BIGSERIAL PRIMARY KEY, org_id BIGINT NOT NULL, revision BIGINT NOT NULL, published BIGINT NOT NULL DEFAULT 0,
    name VARCHAR(128) NOT NULL, purpose TEXT NOT NULL, allowed_tasks TEXT NOT NULL, forbidden TEXT NOT NULL, data_boundary TEXT NOT NULL
);
CREATE INDEX idx_security_profiles_org ON security_profiles(org_id);
CREATE TABLE security_profile_versions (
    id BIGSERIAL PRIMARY KEY, profile_id BIGINT NOT NULL, version BIGINT NOT NULL, actor VARCHAR(255) NOT NULL,
    created_at TIMESTAMP NOT NULL, name VARCHAR(128) NOT NULL, purpose TEXT NOT NULL, allowed_tasks TEXT NOT NULL, forbidden TEXT NOT NULL, data_boundary TEXT NOT NULL, UNIQUE(profile_id, version)
);
CREATE TABLE security_profile_bindings (api_key_id BIGINT PRIMARY KEY, profile_id BIGINT NOT NULL);
