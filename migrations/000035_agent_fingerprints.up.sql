-- Agent fingerprint library: stores fingerprint rules for AI coding tool detection.
-- Replaces hardcoded defaultAgentPatterns in agent_fingerprint.go.
-- Origin types: builtin (seeded on startup), discovered (auto-discovered), manual (admin-added).
-- Status: active (used by engine), pending (awaiting review), ignored (disabled).

CREATE TABLE agent_fingerprints (
    id               BIGSERIAL PRIMARY KEY,
    name             VARCHAR(64)  NOT NULL,
    source_type      VARCHAR(16)  NOT NULL DEFAULT 'header',
    source_field     VARCHAR(128) NOT NULL DEFAULT '',
    pattern          TEXT         NOT NULL,
    risk_level       VARCHAR(16)  NOT NULL DEFAULT 'medium',
    origin           VARCHAR(16)  NOT NULL,
    status           VARCHAR(16)  NOT NULL DEFAULT 'active',
    hit_count        BIGINT       NOT NULL DEFAULT 0,
    last_hit_at      TIMESTAMPTZ,
    discovered_from  JSONB,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_fingerprints_status ON agent_fingerprints(status);
CREATE INDEX idx_agent_fingerprints_name ON agent_fingerprints(name);
CREATE INDEX idx_agent_fingerprints_origin ON agent_fingerprints(origin);
CREATE INDEX idx_agent_fingerprints_status_type ON agent_fingerprints(status, source_type, source_field);

CREATE UNIQUE INDEX idx_agent_fingerprints_dedup
    ON agent_fingerprints(source_type, source_field, md5(pattern));

-- RBAC: fingerprint management actions (Pro/Enterprise)
INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'fingerprint:list'
FROM roles r WHERE r.name IN ('admin', 'member', 'viewer')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'fingerprint:view'
FROM roles r WHERE r.name IN ('admin', 'member', 'viewer')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'fingerprint:manage'
FROM roles r WHERE r.name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;
